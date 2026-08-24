/* FR-41 — the page owns the connection.
 *
 * "Owns" means lifetime, not construction. The peer connection is built by
 * the rendezvous /p bootstrap (FR-33) and handed here; AT-FR-15-b forbids the
 * constructor name from every module the laptop serves, and FR-41 forbids any
 * context that could outlive the page — no worker, no service worker, no
 * shared context. What this module owns is everything that must die with the
 * page: the in-flight request registry, the FR-24 transport state, and the
 * listeners.
 *
 * The wire format is injected. internal/remote/codec speaks a binary framing
 * whose browser half is a separate concern with its own oracle; the lifecycle
 * rules below hold whatever encodes the bytes. The seam mirrors codec.Request
 * and codec.Response as they actually are:
 *
 *   codec.encodeRequest({id, method, path, query, headers, body})
 *       -> an iterable of chunks to write to the channel
 *   codec.receive(data)
 *       -> an iterable of events, each {type, id, ...}
 *            {type:"response", id, status, headers}
 *            {type:"body",     id, chunk}
 *            {type:"end",      id}
 *            {type:"reject",   id, class, message}
 *
 * IDs are strings and bodies are streamed because codec.Request.ID is a
 * string and codec.Response.Body is an io.ReadCloser carried in 0x02/0x03
 * frames. A seam that collapsed either is a seam the real codec cannot
 * satisfy.
 *
 * Deliberately absent: a per-request timeout. A request that is never
 * answered over a channel that stays open is not this layer's problem to
 * guess at — the protocol has a cancel frame, and abandoning a request here
 * without sending one would leave the laptop working on an answer nobody will
 * read. Loss is what this module settles, and it settles it completely.
 */

export const CAUSE_RENDEZVOUS_UNAVAILABLE = "rendezvous-unavailable";
export const CAUSE_LAPTOP_OFFLINE = "laptop-offline";
export const CAUSE_SIGNALLING_REJECTED = "signalling-rejected";
export const CAUSE_ICE_FAILED = "ice-failed";
export const CAUSE_AUTH_FAILED = "auth-failed";
export const CAUSE_CONNECTED_THEN_LOST = "connected-then-lost";

/* The name every rejection caused by transport loss carries. Callers
   distinguish it from an HTTP error, which resolves as a response. */
export const LOST = "remote-connection-lost";

function lostError(cause) {
  const err = new Error("the connection to the laptop was lost");
  err.name = LOST;
  err.cause = cause;
  return err;
}

/* A minimal Headers stand-in over the decoded map. api.js reads
   headers.get("content-type") on every call and HTTP header names are
   case-insensitive, so a plain object would silently miss "Content-Type". */
function headersOf(raw) {
  const map = new Map();
  for (const [k, v] of Object.entries(raw || {})) map.set(String(k).toLowerCase(), v);
  return {
    get(name) {
      const k = String(name).toLowerCase();
      return map.has(k) ? map.get(k) : null;
    },
  };
}

/* Chunks arrive as strings or as bytes depending on the codec. Join them
   without assuming which, and only once the body has ended. */
function joinChunks(chunks) {
  if (chunks.every(c => typeof c === "string")) return chunks.join("");
  const parts = chunks.map(c => (typeof c === "string" ? new TextEncoder().encode(c) : new Uint8Array(c)));
  const total = parts.reduce((n, p) => n + p.length, 0);
  const out = new Uint8Array(total);
  let at = 0;
  for (const p of parts) {
    out.set(p, at);
    at += p.length;
  }
  return out;
}

function textOf(joined) {
  return typeof joined === "string" ? joined : new TextDecoder().decode(joined);
}

/* splitTarget separates codec.Request's Path from its Query. The codec keeps
   them apart, so collapsing them here would be undone downstream anyway. */
function splitTarget(target) {
  const s = String(target || "");
  const q = s.indexOf("?");
  return q < 0 ? { path: s, query: "" } : { path: s.slice(0, q), query: s.slice(q + 1) };
}

/* Headers reach fetchImpl as a Headers instance (api.js builds one) or as a
   plain object. codec.Request.Headers is a map, so normalize. */
function plainHeaders(h) {
  if (!h) return {};
  if (typeof h.forEach === "function" && typeof h.get === "function") {
    const out = {};
    h.forEach((v, k) => {
      out[k] = v;
    });
    return out;
  }
  return { ...h };
}

/* createChannelTransport wires an already-established peer and data channel
   into the FR-42 fetchImpl seam.

   deps.peer      the peer connection, injected (connectionState + events)
   deps.channel   the open data channel, injected
   deps.codec     { encodeRequest(req), receive(data) } — the wire format
   deps.reconnect called only by reconnect(); nothing here reconnects itself
   deps.onState   notified on every transport state transition */
export function createChannelTransport(deps) {
  const { peer, channel, codec, reconnect = () => {}, onState = () => {} } = deps;

  /* id -> {settleHead, failHead, chunks, endBody, failBody, headDone} */
  const pending = new Map();
  let nextID = 1;
  let cause = "";
  let closed = false;

  const state = () => ({ connected: !closed && cause === "", cause });

  /* Settle every outstanding request and stop. FR-41's whole point: at the
     moment of loss nothing is left waiting for an answer that cannot come —
     including a response whose head arrived and whose body is still in
     flight, where the hang would otherwise just move to res.text(). */
  function settleAllLost(next) {
    const waiting = [...pending.values()];
    pending.clear();
    for (const p of waiting) {
      if (p.headDone) p.failBody(lostError(next));
      else p.failHead(lostError(next));
    }
  }

  function fail(next) {
    if (cause !== "" || closed) return;
    cause = next;
    settleAllLost(next);
    onState(state());
  }

  const onMessage = ev => {
    let events;
    try {
      events = codec.receive(ev.data);
    } catch {
      /* An undecodable frame is not a reason to tear down a working channel;
         the request it belonged to stays pending until loss settles it. */
      return;
    }
    for (const e of events || []) handleEvent(e);
  };

  function handleEvent(e) {
    if (!e || e.id == null) return;
    const p = pending.get(String(e.id));
    /* No entry means the request was already settled by loss. A late answer
       is dropped, never re-settled. */
    if (!p) return;

    switch (e.type) {
      case "response":
        if (p.headDone) return;
        p.headDone = true;
        p.settleHead(e);
        return;
      case "body":
        if (e.chunk != null) p.chunks.push(e.chunk);
        return;
      case "end":
        pending.delete(String(e.id));
        if (!p.headDone) {
          /* End without a head: nothing can be returned. Treat it as loss of
             that request rather than resolving an empty response. */
          p.failHead(lostError(cause || CAUSE_CONNECTED_THEN_LOST));
          return;
        }
        p.endBody();
        return;
      case "reject": {
        pending.delete(String(e.id));
        const err = new Error(e.message || "rejected");
        err.name = "remote-rejected";
        err.class = e.class || "";
        if (p.headDone) p.failBody(err);
        else p.failHead(err);
        return;
      }
      default:
        /* Unknown event types are ignored rather than fatal: the codec may
           learn frames this layer has no opinion about. */
        return;
    }
  }

  const onChannelClose = () => fail(CAUSE_CONNECTED_THEN_LOST);
  const onChannelError = () => fail(CAUSE_CONNECTED_THEN_LOST);

  const onPeerState = () => {
    /* "failed" is ICE giving up — the one state FR-24 lets us name the
       SSH/WireGuard/Tailscale fallback for. "disconnected" is routinely
       transient and recovers on its own, so it is deliberately not loss. */
    if (peer.connectionState === "failed") fail(CAUSE_ICE_FAILED);
    else if (peer.connectionState === "closed") fail(CAUSE_CONNECTED_THEN_LOST);
  };

  channel.addEventListener("message", onMessage);
  channel.addEventListener("close", onChannelClose);
  channel.addEventListener("error", onChannelError);
  peer.addEventListener("connectionstatechange", onPeerState);

  function detach() {
    channel.removeEventListener("message", onMessage);
    channel.removeEventListener("close", onChannelClose);
    channel.removeEventListener("error", onChannelError);
    peer.removeEventListener("connectionstatechange", onPeerState);
  }

  function fetchImpl(target, opts) {
    if (closed || cause !== "") {
      /* Reject rather than write into a dead channel: a request that can
         never be answered must not be left outstanding. Note the refusal
         comes from our own FR-24 state, not from channel.send() throwing —
         after ICE failure the channel can still report readyState "open". */
      return Promise.reject(lostError(cause || CAUSE_CONNECTED_THEN_LOST));
    }

    const o = opts || {};
    const id = String(nextID++);
    const { path, query } = splitTarget(target);
    const req = {
      id,
      method: o.method ? String(o.method).toUpperCase() : "GET",
      path,
      query,
      headers: plainHeaders(o.headers),
      body: o.body == null ? "" : o.body,
    };

    const entry = { chunks: [], headDone: false };
    const bodyPromise = new Promise((resolve, reject) => {
      entry.endBody = () => resolve(joinChunks(entry.chunks));
      entry.failBody = reject;
    });
    /* Nothing awaits bodyPromise until the caller reads the body; without
       this an unread rejected body is an unhandled rejection. */
    bodyPromise.catch(() => {});

    const headPromise = new Promise((resolve, reject) => {
      entry.settleHead = head => resolve(responseOf(head, bodyPromise));
      entry.failHead = reject;
    });

    pending.set(id, entry);
    try {
      for (const chunk of codec.encodeRequest(req)) channel.send(chunk);
    } catch (err) {
      pending.delete(id);
      /* The channel died between the check and the write. Report it as loss,
         and settle everything else the same way. */
      entry.failHead(lostError(CAUSE_CONNECTED_THEN_LOST));
      fail(CAUSE_CONNECTED_THEN_LOST);
    }
    return headPromise;
  }

  function responseOf(head, bodyPromise) {
    const status = head.status || 0;
    return {
      ok: status >= 200 && status < 300,
      status,
      headers: headersOf(head.headers),
      text: () => bodyPromise.then(textOf),
      json: () => bodyPromise.then(b => JSON.parse(textOf(b))),
      arrayBuffer: () =>
        bodyPromise.then(b => (typeof b === "string" ? new TextEncoder().encode(b).buffer : b.buffer)),
    };
  }

  return {
    fetchImpl,
    state,
    pendingCount: () => pending.size,

    /* Explicit only. Loss never triggers this; a human does. */
    reconnect: (...args) => reconnect(...args),

    /* Teardown for pagehide/unload and for replacing the transport. Nothing
       survives it: no listener, no pending request, no unfinished body. */
    close() {
      if (closed) return;
      const c = cause || CAUSE_CONNECTED_THEN_LOST;
      closed = true;
      detach();
      settleAllLost(c);
      onState(state());
    },
  };
}
