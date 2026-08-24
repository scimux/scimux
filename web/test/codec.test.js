/* Browser codec vs the Go production-encoder oracle
 * (internal/remote/codec/testdata/vectors.json). */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { createCodec, encodeRequest } from "../js/codec.js";

const HERE = dirname(fileURLToPath(import.meta.url));
const VECTOR_PATH = join(HERE, "../../internal/remote/codec/testdata/vectors.json");
const doc = JSON.parse(readFileSync(VECTOR_PATH, "utf8"));

function b64(u8) {
  return Buffer.from(u8).toString("base64");
}

function unb64(s) {
  return Uint8Array.from(Buffer.from(s, "base64"));
}

function concat(chunks) {
  const parts = [...chunks].map(c => (c instanceof Uint8Array ? c : new Uint8Array(c)));
  const n = parts.reduce((a, p) => a + p.length, 0);
  const out = new Uint8Array(n);
  let at = 0;
  for (const p of parts) {
    out.set(p, at);
    at += p.length;
  }
  return out;
}

function requestArg(req) {
  const out = {
    id: req.id,
    method: req.method,
    path: req.path,
    query: req.query,
    headers: req.headers || {},
    body: req.body || "",
  };
  if (req.body_b64) out.body = unb64(req.body_b64);
  return out;
}

function serialize(events) {
  return (events || []).map(e => {
    const row = { type: e.type, id: String(e.id) };
    if (e.type === "response") {
      row.status = e.status;
      row.headers = e.headers || {};
    }
    if (e.type === "body") row.chunk_b64 = b64(e.chunk instanceof Uint8Array ? e.chunk : new Uint8Array(e.chunk || []));
    if (e.type === "reject") {
      row.class = e.class;
      row.message = e.message;
    }
    return row;
  });
}

function expectedEvents(vec) {
  return (vec.events || []).map(e => {
    const row = { type: e.type, id: String(e.id) };
    if (e.type === "response") {
      row.status = e.status;
      row.headers = e.headers || {};
    }
    if (e.type === "body") row.chunk_b64 = e.chunk_b64;
    if (e.type === "reject") {
      row.class = e.class;
      row.message = e.message;
    }
    return row;
  });
}

function collect(codec, data) {
  return [...(codec.receive(data) || [])];
}

function collectBytes(codec, bytes) {
  const out = [];
  for (const b of bytes) {
    for (const e of codec.receive(new Uint8Array([b])) || []) out.push(e);
  }
  return out;
}

test("anti-vacuity: vector and per-type counts", () => {
  assert.ok(Array.isArray(doc.vectors) && doc.vectors.length > 0, "vectors.json is empty");
  assert.equal(doc.vectors.length, doc.counts.vectors, "counts.vectors does not match the array");
  const byType = { response: 0, body: 0, end: 0, reject: 0 };
  let nReq = 0;
  for (const v of doc.vectors) {
    if (v.request) nReq++;
    for (const e of v.events || []) {
      if (byType[e.type] != null) byType[e.type]++;
    }
  }
  assert.equal(nReq, doc.counts.request);
  assert.equal(byType.response, doc.counts.response);
  assert.equal(byType.body, doc.counts.body);
  assert.equal(byType.end, doc.counts.end);
  assert.equal(byType.reject, doc.counts.reject);
  assert.equal(doc.counts.vectors, 11);
  assert.equal(doc.counts.request, 4);
  assert.equal(doc.counts.response, 3);
  assert.equal(doc.counts.body, 6);
  assert.equal(doc.counts.end, 8);
  assert.equal(doc.counts.reject, 2);
});

test("every request vector's bytes are reproduced exactly by encodeRequest", () => {
  const reqs = doc.vectors.filter(v => v.request);
  assert.ok(reqs.length >= 4, "no request vectors to encode");
  for (const v of reqs) {
    const got = concat(encodeRequest(requestArg(v.request)));
    assert.equal(b64(got), v.bytes, `${v.name}: encodeRequest bytes differ from the oracle`);
  }
});

test("every vector's frames decode via receive into the expected events", () => {
  assert.ok(doc.vectors.length > 0);
  for (const v of doc.vectors) {
    const c = createCodec();
    const got = serialize(collect(c, unb64(v.bytes)));
    assert.deepEqual(got, expectedEvents(v), `${v.name}: receive events differ from the oracle`);
  }
});

test("receive is stateful across calls: one byte at a time matches the whole vector", () => {
  assert.ok(doc.vectors.length > 0);
  for (const v of doc.vectors) {
    const bytes = unb64(v.bytes);
    const whole = serialize(collect(createCodec(), bytes));
    const piecemeal = serialize(collectBytes(createCodec(), bytes));
    assert.deepEqual(piecemeal, whole, `${v.name}: byte-at-a-time events differ from feeding the vector whole`);
    assert.deepEqual(piecemeal, expectedEvents(v), `${v.name}: byte-at-a-time events differ from the oracle`);
  }
});

test("two frames arriving in one message yield both events from one call", () => {
  const body = doc.vectors.find(v => v.name === "body-only");
  const end = doc.vectors.find(v => v.name === "end-only");
  assert.ok(body && end, "oracle is missing body-only/end-only frames");
  const joined = concat([unb64(body.bytes), unb64(end.bytes)]);
  const c = createCodec();
  const got = serialize(collect(c, joined));
  const want = expectedEvents(body).concat(expectedEvents(end));
  assert.equal(got.length, 2, "one receive() over two frames did not yield two events");
  assert.deepEqual(got, want);
});

test("a declared length above maxFramePayload is refused without allocating it", () => {
  const cHuge = createCodec();
  const hugeHdr = new Uint8Array([0x00, 0xff, 0xff, 0xff]);
  const heapBefore = process.memoryUsage().heapUsed;
  const first = collect(cHuge, hugeHdr);
  const heapAfter = process.memoryUsage().heapUsed;
  assert.equal(first.length, 0, "oversized length must not decode a frame from the header alone");
  assert.ok(
    heapAfter - heapBefore < 2 * 1024 * 1024,
    `uint24-max declared length was allocated (heap +${heapAfter - heapBefore})`,
  );

  const max = doc.max_frame_payload;
  const len = max + 1;
  const c = createCodec();
  const hdr = new Uint8Array([0x00, (len >> 16) & 0xff, (len >> 8) & 0xff, len & 0xff]);
  collect(c, hdr);
  let threw = null;
  const junk = new Uint8Array(1024);
  try {
    let left = len;
    while (left > 0) {
      const n = Math.min(junk.length, left);
      collect(c, junk.subarray(0, n));
      left -= n;
    }
  } catch (err) {
    threw = err;
  }
  assert.ok(threw, "oversized frame was accepted");
});

test("a body larger than bodyChunkSize is emitted as multiple body frames, then one end", () => {
  const size = doc.body_chunk_size;
  assert.ok(size > 0, "oracle body_chunk_size is missing");
  const body = new Uint8Array(size + 1).fill(0x61);
  const frames = concat(
    encodeRequest({
      id: "c",
      method: "POST",
      path: "/api/ui",
      query: "",
      headers: { "Content-Type": "application/json" },
      body,
    }),
  );
  const events = serialize(collect(createCodec(), frames));
  const bodies = events.filter(e => e.type === "body");
  assert.ok(bodies.length >= 2, `body of ${size + 1} produced ${bodies.length} body event(s); want multiple`);
  for (const b of bodies) {
    const n = unb64(b.chunk_b64).length;
    assert.ok(n <= size, `body chunk ${n} exceeds bodyChunkSize ${size}`);
  }
  assert.equal(events[events.length - 1].type, "end");
  assert.equal(events.filter(e => e.type === "end").length, 1);
});

/* Review addition — decoder state must not be shared.
 *
 * The decoder holds a partial-frame buffer, so two streams sharing one
 * instance corrupt each other. The FR-41 reconnect path makes that concrete:
 * a new transport built after loss and handed a shared decoder inherits the
 * dead channel's trailing half-frame and decodes garbage from its first
 * message. Neither symptom looks like a codec bug from the outside.
 */
test("each createCodec() carries its own decode state", () => {
  const v = doc.vectors.find(x => x.events.length > 1);
  assert.ok(v, "anti-vacuity: no multi-event vector");
  const bytes = unb64(v.bytes);
  assert.ok(bytes.length > 2, "anti-vacuity: vector too short to split");

  const a = createCodec();
  const b = createCodec();

  // Strand A mid-frame, then drive B to completion.
  a.receive(bytes.subarray(0, 2));
  const fromB = [...b.receive(bytes)];
  const whole = [...createCodec().receive(bytes)];

  assert.deepEqual(
    fromB,
    whole,
    "a codec's output changed because another instance was mid-frame; decode state is shared",
  );
});

test("the module exports no shared decoder", async () => {
  const mod = await import("../js/codec.js");
  assert.equal(
    typeof mod.receive,
    "undefined",
    "codec.js exports a module-level receive() backed by one shared buffer. Two transports — or a transport built after a reconnect — would interleave into it. createCodec() must be the only way to get a decoder.",
  );
});
