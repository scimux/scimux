/* FR-27 browser codec. Wire format is the Go oracle in
 * internal/remote/codec/testdata/vectors.json; this file satisfies the
 * seam connection.js already consumes.
 *
 *   encodeRequest({id, method, path, query, headers, body})
 *       -> iterable of chunks to write to the data channel
 *   receive(data)
 *       -> iterable of {type:"response"|"body"|"end"|"reject", ...}
 *
 * Cancel frames (0x04) are parsed and dropped; nothing calls them yet.
 */

const TYPE_REQUEST = 0x00;
const TYPE_RESPONSE = 0x01;
const TYPE_BODY = 0x02;
const TYPE_BODY_END = 0x03;
const TYPE_CANCEL = 0x04;
const TYPE_REJECT = 0x05;

const FRAME_HEADER_SIZE = 4;
const MAX_FRAME_PAYLOAD = (64 << 10) + 1024;
const BODY_CHUNK_SIZE = 32 << 10;
const MAX_STRING = 1 << 16;

const utf8 = new TextEncoder();
const utf8dec = new TextDecoder();

function rejectMalformed() {
  const e = new Error("codec: rejected malformed");
  e.class = "malformed";
  throw e;
}

function toU8(data) {
  if (data == null) return new Uint8Array(0);
  if (data instanceof Uint8Array) return data;
  if (data instanceof ArrayBuffer) return new Uint8Array(data);
  if (ArrayBuffer.isView(data)) return new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
  if (typeof data === "string") return utf8.encode(data);
  return new Uint8Array(data);
}

function concatU8(parts) {
  let n = 0;
  for (const p of parts) n += p.length;
  const out = new Uint8Array(n);
  let at = 0;
  for (const p of parts) {
    out.set(p, at);
    at += p.length;
  }
  return out;
}

function encodeString(s) {
  const b = utf8.encode(s == null ? "" : String(s));
  if (b.length >= MAX_STRING) rejectMalformed();
  const out = new Uint8Array(2 + b.length);
  out[0] = (b.length >> 8) & 0xff;
  out[1] = b.length & 0xff;
  out.set(b, 2);
  return out;
}

function encodeHeaders(headers) {
  const entries = Object.entries(headers || {});
  const parts = [u16(entries.length)];
  for (const [name, val] of entries) {
    parts.push(encodeString(name));
    const vals = Array.isArray(val) ? val : [val];
    parts.push(u16(vals.length));
    for (const v of vals) parts.push(encodeString(v == null ? "" : String(v)));
  }
  return concatU8(parts);
}

function u16(n) {
  return new Uint8Array([(n >> 8) & 0xff, n & 0xff]);
}

function frame(typ, payload) {
  const n = payload.length;
  if (n > 0xffffff) rejectMalformed();
  const out = new Uint8Array(FRAME_HEADER_SIZE + n);
  out[0] = typ;
  out[1] = (n >> 16) & 0xff;
  out[2] = (n >> 8) & 0xff;
  out[3] = n & 0xff;
  if (n) out.set(payload, FRAME_HEADER_SIZE);
  return out;
}

function bodyBytes(body) {
  if (body == null || body === "") return null;
  return toU8(body);
}

export function encodeRequest(req) {
  const id = String(req.id);
  const payload = concatU8([
    encodeString(id),
    encodeString(req.method),
    encodeString(req.path),
    encodeString(req.query),
    encodeHeaders(req.headers),
  ]);
  const frames = [frame(TYPE_REQUEST, payload)];
  const idEnc = encodeString(id);
  const body = bodyBytes(req.body);
  if (body && body.length) {
    for (let off = 0; off < body.length; ) {
      const n = Math.min(BODY_CHUNK_SIZE, body.length - off);
      frames.push(frame(TYPE_BODY, concatU8([idEnc, body.subarray(off, off + n)])));
      off += n;
    }
  }
  frames.push(frame(TYPE_BODY_END, idEnc));
  return frames;
}

function takeU16(p) {
  if (p.length < 2) rejectMalformed();
  return [(p[0] << 8) | p[1], p.subarray(2)];
}

function takeString(p) {
  const [n, rest] = takeU16(p);
  if (n > rest.length) rejectMalformed();
  return [utf8dec.decode(rest.subarray(0, n)), rest.subarray(n)];
}

function takeHeaders(p) {
  const [n, rest0] = takeU16(p);
  const headers = {};
  let rest = rest0;
  for (let i = 0; i < n; i++) {
    const [name, r1] = takeString(rest);
    const [nv, r2] = takeU16(r1);
    const vals = [];
    rest = r2;
    for (let j = 0; j < nv; j++) {
      const [v, r3] = takeString(rest);
      vals.push(v);
      rest = r3;
    }
    headers[name] = vals.length <= 1 ? (vals[0] || "") : vals;
  }
  return [headers, rest];
}

function rejectMessage(cls, field) {
  if (field) return `codec: rejected ${cls} in ${field}`;
  return `codec: rejected ${cls}`;
}

function decodePayload(typ, payload) {
  switch (typ) {
    case TYPE_REQUEST:
    case TYPE_CANCEL:
      return null;
    case TYPE_RESPONSE: {
      const [id, p1] = takeString(payload);
      const [status, p2] = takeU16(p1);
      if (status < 100 || status > 599) rejectMalformed();
      const [headers] = takeHeaders(p2);
      return { type: "response", id, status, headers };
    }
    case TYPE_BODY: {
      const [id, rest] = takeString(payload);
      return { type: "body", id, chunk: rest.slice() };
    }
    case TYPE_BODY_END: {
      const [id] = takeString(payload);
      return { type: "end", id };
    }
    case TYPE_REJECT: {
      const [id, p1] = takeString(payload);
      const [cls, p2] = takeString(p1);
      const [field] = takeString(p2);
      return { type: "reject", id, class: cls, message: rejectMessage(cls, field) };
    }
    default:
      rejectMalformed();
  }
}

function createState() {
  return { buf: new Uint8Array(0), off: 0, discard: 0 };
}

function append(st, data) {
  const add = toU8(data);
  if (!add.length) return;
  if (st.off > 0) {
    st.buf = st.buf.subarray(st.off);
    st.off = 0;
  }
  const next = new Uint8Array(st.buf.length + add.length);
  next.set(st.buf);
  next.set(add, st.buf.length);
  st.buf = next;
}

function avail(st) {
  return st.buf.length - st.off;
}

function skip(st, n) {
  st.off += n;
}

function view(st, n) {
  return st.buf.subarray(st.off, st.off + n);
}

function pullEvents(st, data) {
  append(st, data);
  const events = [];
  for (;;) {
    if (st.discard > 0) {
      const n = Math.min(st.discard, avail(st));
      skip(st, n);
      st.discard -= n;
      if (st.discard > 0) return events;
      rejectMalformed();
    }
    if (avail(st) < 1) return events;
    const typ = st.buf[st.off];
    if (typ > TYPE_REJECT) {
      skip(st, 1);
      rejectMalformed();
    }
    if (avail(st) < FRAME_HEADER_SIZE) return events;
    const hdr = view(st, FRAME_HEADER_SIZE);
    const length = (hdr[1] << 16) | (hdr[2] << 8) | hdr[3];
    if (length > MAX_FRAME_PAYLOAD) {
      skip(st, FRAME_HEADER_SIZE);
      st.discard = length;
      continue;
    }
    if (avail(st) < FRAME_HEADER_SIZE + length) return events;
    skip(st, FRAME_HEADER_SIZE);
    const payload = view(st, length).slice();
    skip(st, length);
    const ev = decodePayload(typ, payload);
    if (ev) events.push(ev);
  }
}

export function createCodec() {
  const st = createState();
  return {
    encodeRequest,
    receive(data) {
      return pullEvents(st, data);
    },
  };
}

/* Deliberately no module-level receive(). The decoder holds a partial-frame
   buffer, so one shared instance is a correctness hazard, not a convenience:
   two transports would interleave into it, and a transport built after an
   FR-41 reconnect would inherit the dead channel's trailing half-frame and
   decode garbage from its first message. createCodec() is the only way to
   get a decoder; encodeRequest is stateless and stays a plain export. */
