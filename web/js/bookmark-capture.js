"use strict";

import { hasSupportedImageMarker } from "./reference-media.js";

function clone(value){
  if (value === undefined) return undefined;
  return JSON.parse(JSON.stringify(value));
}

function exactAddress(turn){
  if (!turn || typeof turn.uid !== "string" || !turn.uid) return null;
  // Chat Turn JSON omits zero ordinals. The capture POST requires explicit
  // numbers, so restore those zeros without accepting malformed values.
  const segment = turn.segment === undefined ? 0 : turn.segment;
  const record = turn.record === undefined ? 0 : turn.record;
  if (!Number.isInteger(segment) || segment < 0 ||
      !Number.isInteger(record) || record < 0) return null;
  return { uid: turn.uid, segment, record };
}

export function createBookmarkCapture({ api }){
  if (typeof api !== "function") throw new TypeError("bookmark capture requires api");
  const pending = new Map();
  let generation = 0;

  function capture(bookmark, turn){
    const base = clone(bookmark) || {};
    if (turn && Object.prototype.hasOwnProperty.call(turn, "prov")) base.prov = clone(turn.prov);
    if (!hasSupportedImageMarker(base.text)) return base;
    const source = exactAddress(turn);
    if (!source) throw new Error("image bookmark has no durable source address");
    const key = `${source.uid}:${source.segment}:${source.record}`;
    const ownGeneration = generation;
    let request = pending.get(key);
    if (!request) {
      request = Promise.resolve(api("/api/reference-media", { method:"POST", body:JSON.stringify({ source }) }));
      pending.set(key, request);
      request.finally(() => { if (pending.get(key) === request) pending.delete(key); }).catch(() => {});
    }
    return request.then(durable => {
      if (ownGeneration !== generation) throw new Error("bookmark capture cancelled");
      const out = clone(base);
      out.text = durable.text;
      if (Array.isArray(durable.media?.items) && durable.media.items.length) out.media = clone(durable.media);
      return out;
    });
  }

  return { capture, clear(){ generation++; pending.clear(); } };
}
