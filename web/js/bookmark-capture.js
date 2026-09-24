"use strict";

import { hasSupportedImageMarker } from "./reference-media.js";

function clone(value){
  if (value === undefined) return undefined;
  return JSON.parse(JSON.stringify(value));
}

function exactAddress(turn){
  if (!turn || typeof turn.uid !== "string" || !turn.uid ||
      !Object.prototype.hasOwnProperty.call(turn, "segment") ||
      !Object.prototype.hasOwnProperty.call(turn, "record") ||
      !Number.isInteger(turn.segment) || turn.segment < 0 ||
      !Number.isInteger(turn.record) || turn.record < 0) return null;
  return { uid: turn.uid, segment: turn.segment, record: turn.record };
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
