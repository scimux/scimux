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
 * The wire format is injected too. internal/remote/codec speaks a binary
 * framing whose browser half is a separate concern with its own oracle; the
 * lifecycle rules below are the same whatever encodes the bytes.
 *
 * Deliberately absent: a per-request timeout. A request that is never answered
 * over a channel that stays open is not this layer's problem to guess at — the
 * protocol has a cancel frame, and abandoning a request here without sending
 * one would leave the laptop working on an answer nobody will read. Loss is
 * what this module settles, and it settles it completely.
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
  return { get: name => (map.has(String(name).toLowerCase()) ? map.get(String(name).toLowerCase()) : null) };
}

function responseOf(msg) {
  const status = msg.status || 0;
  const body = msg.body == null ? "" : msg.body;
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: headersOf(msg.headers),
    text: async () => body,
    json: async () => JSON.parse(body),
    arrayBuffer: async () => (typeof body === "string" ? new TextEncoder().encode(body).buffer : body),
  };
}

/* createChannelTransport wires an already-established peer and data channel
   into the FR-42 fetchImpl seam.

   deps.peer      the peer connection, injected (connectionState + events)
   deps.channel   the open data channel, injected
   deps.codec     { encode(request), decode(data) } — the wire format
   deps.reconnect called only by reconnect(); nothing here reconnects itself
   deps.onState   notified on every transport state transition */
export function createChannelTransport(deps) {
  const { peer, channel, codec, reconnect = () => {}, onState = () => {} } = deps;

  const pending = new Map();
  let nextID = 1;
  let cause = "";
  let closed = false;

  const state = () => ({ connected: !closed && cause === "", cause });

  /* Settle every outstanding request and stop. FR-41's whole point: at the
     moment of loss nothing is left waiting for an answer that cannot come. */
  function fail(next) {
    if (cause !== "" || closed) return;
    cause = next;
    const waiting = [...pending.values()];
    pending.clear();
    for (const p of waiting) p.reject(lostError(next));
    onState(state());
  }

  const onMessage = ev => {
    let msg;
    try {
      msg = codec.decode(ev.data);
    } catch {
      /* An undecodable frame is not a reason to tear down a working channel;
         the request it belonged to stays pending until loss settles it. */
      return;
    }
    const p = pending.get(msg && msg.id);
    /* No entry means the request was already settled by loss. A late answer
       is dropped, never re-settled. */
    if (!p) return;
    pending.delete(msg.id);
    p.resolve(responseOf(msg));
  };

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

  function fetchImpl(path, opts) {
    if (closed || cause !== "") {
      /* Reject rather than write into a dead channel: a request that can
         never be answered must not be left outstanding. */
      return Promise.reject(lostError(cause || CAUSE_CONNECTED_THEN_LOST));
    }
    const id = nextID++;
    return new Promise((resolve, reject) => {
      pending.set(id, { resolve, reject });
      try {
        channel.send(codec.encode({ id, path, opts: opts || {} }));
      } catch (err) {
        pending.delete(id);
        /* The channel died between the check and the write. Report it as
           loss, and settle everything else the same way. */
        reject(lostError(CAUSE_CONNECTED_THEN_LOST));
        fail(CAUSE_CONNECTED_THEN_LOST);
      }
    });
  }

  return {
    fetchImpl,
    state,
    pendingCount: () => pending.size,

    /* Explicit only. Loss never triggers this; a human does. */
    reconnect: (...args) => reconnect(...args),

    /* Teardown for pagehide/unload and for replacing the transport. Nothing
       survives it: no listener, no pending request. */
    close() {
      if (closed) return;
      const waiting = [...pending.values()];
      pending.clear();
      const c = cause || CAUSE_CONNECTED_THEN_LOST;
      closed = true;
      detach();
      for (const p of waiting) p.reject(lostError(c));
      onState(state());
    },
  };
}
