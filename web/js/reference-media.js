"use strict";

const CAPTURE_RE = /^[0-9a-f]{64}$/;
const ITEM_ID_RE = /^(0|[1-9][0-9]*)$/;
const KEY_RE = /^(asset:[A-Za-z0-9_]+|attachment:[^/\\]+)$/;
const STATE = new Set(["ready", "unavailable"]);
const ASSET_MARKER_RE = /(!?)\[([^\]]*)\]\(scimux-asset:([A-Za-z0-9_]+)\)/g;
const RASTER_NAME_RE = /\.(png|jpe?g|gif|webp)$/i;

function defaultEsc(value){
  return String(value ?? "").replaceAll("&", "&amp;").replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;").replaceAll('"', "&quot;").replaceAll("'", "&#39;");
}

export function validReferenceMedia(media){
  if (!media || media.version !== 1 || !CAPTURE_RE.test(media.capture_id || "") || !Array.isArray(media.items)) return false;
  return media.items.length <= 32 && media.items.every((item, index) => item &&
    item.id === String(index) && ITEM_ID_RE.test(item.id) && KEY_RE.test(item.key || "") &&
    typeof item.name === "string" && item.name.length > 0 && !/[\0\r\n]/.test(item.name) && STATE.has(item.state));
}

function unavailableTile(name, esc){
  return `<span class="attfile missing reference-media-unavailable" role="status"><span>${esc(name || "image")} (unavailable)</span></span>`;
}

export function renderReferenceMedia(media, { esc = defaultEsc, assetURL = path => path } = {}){
  if (!validReferenceMedia(media)) {
    return `<div class="attrow reference-media">${unavailableTile("image", esc)}</div>`;
  }
  const tiles = media.items.map(item => {
    if (item.state !== "ready") return unavailableTile(item.name, esc);
    const url = assetURL(`/api/reference-media/${media.capture_id}/assets/${item.id}`);
    return `<a class="attthumb reference-media-thumb" data-asset-preview data-name="${esc(item.name)}" data-tkey="${esc(item.key)}" href="${url}" title="${esc(item.name)}"><img src="${url}" alt="${esc(item.name)}" loading="lazy"></a>`;
  }).join("");
  return `<div class="attrow reference-media">${tiles}</div>`;
}

export function mediaTextAndTiles(text, media, deps = {}){
  const valid = validReferenceMedia(media);
  const keys = new Set(valid ? media.items.map(item => item.key) : []);
  ASSET_MARKER_RE.lastIndex = 0;
  const clean = String(text || "").replace(ASSET_MARKER_RE, (whole, _bang, _alt, id) =>
    (media && (!valid || keys.has(`asset:${id}`))) ? "" : whole).replace(/\n{3,}/g, "\n\n").trim();
  return { clean, html: renderReferenceMedia(media, deps) };
}

export function hasSupportedImageMarker(text){
  ASSET_MARKER_RE.lastIndex = 0;
  return ASSET_MARKER_RE.test(String(text || ""));
}

function durableAddress(record){
  if (!record || typeof record.uid !== "string" || !record.uid ||
      !Object.prototype.hasOwnProperty.call(record, "segment") ||
      !Object.prototype.hasOwnProperty.call(record, "record") ||
      !Number.isInteger(record.segment) || record.segment < 0 ||
      !Number.isInteger(record.record) || record.record < 0) return null;
  return { uid:record.uid, segment:record.segment, record:record.record };
}

export function createLegacyMediaResolver({ api, onUpdate = () => {}, maxEntries = 64, AbortControllerImpl = globalThis.AbortController } = {}){
  if (typeof api !== "function") throw new TypeError("legacy media resolver requires api");
  const cache = new Map();
  const pending = new Set();
  let generation = 0;

  function view(record){
    const address = durableAddress(record);
    if (!address) return { state:"unavailable" };
    const key = `${address.uid}:${address.segment}:${address.record}`;
    if (cache.has(key)) return cache.get(key);
    if (cache.size >= maxEntries) return { state:"unavailable" };
    const controller = AbortControllerImpl ? new AbortControllerImpl() : null;
    const entry = { state:"pending", controller };
    cache.set(key, entry);
    const ownGeneration = generation;
    const path = `/api/preview?uid=${encodeURIComponent(address.uid)}&seg=${address.segment}&rec=${address.record}`;
    const promise = Promise.resolve(api(path, controller ? { signal:controller.signal } : undefined)).then(data => {
      if (ownGeneration !== generation || cache.get(key) !== entry) return;
      const turn = data && Array.isArray(data.turns) ? data.turns[data.anchor] : null;
      // Preview uses the same Turn JSON as chat: zero ordinals are omitted.
      const segment = turn?.segment === undefined ? 0 : turn.segment;
      const record = turn?.record === undefined ? 0 : turn.record;
      const exact = data?.uid === address.uid && turn?.uid === address.uid &&
        segment === address.segment && record === address.record && typeof data.node === "string" && data.node;
      cache.set(key, exact ? { state:"ready", node:data.node, assets:data.assets || {} } : { state:"unavailable" });
    }, () => {
      if (ownGeneration === generation && cache.get(key) === entry) cache.set(key, { state:"unavailable" });
    }).finally(() => {
      pending.delete(promise);
      if (ownGeneration === generation) onUpdate(key);
    });
    pending.add(promise);
    return entry;
  }
  function destroy(){
    generation++;
    for (const entry of cache.values()) entry.controller?.abort?.();
    cache.clear(); pending.clear();
  }
  return { view, destroy, settled:() => Promise.allSettled([...pending]), size:() => cache.size };
}

export function legacyMediaTextAndTiles(record, resolver, { esc = defaultEsc, assetURL = path => path } = {}){
  const markers = [];
  const seen = new Set();
  const state = resolver?.view?.(record) || { state:"unavailable" };
  ASSET_MARKER_RE.lastIndex = 0;
  const clean = String(record?.text || "").replace(ASSET_MARKER_RE, (whole, bang, alt, id) => {
    const asset = state.state === "ready" ? state.assets?.[id] : null;
    const name = typeof asset?.name === "string" ? asset.name : alt || "image";
    const image = bang === "!" || RASTER_NAME_RE.test(name);
    if (!image) return whole;
    if (!seen.has(id)) { seen.add(id); markers.push({ id, name, asset }); }
    return "";
  }).replace(/\n{3,}/g, "\n\n").trim();
  if (!markers.length) return { clean:String(record?.text || ""), html:"" };
  const tiles = markers.map(marker => {
    const { asset, name } = marker;
    if (!asset || !RASTER_NAME_RE.test(name)) return unavailableTile(name, esc);
    const url = assetURL(`/api/nodes/${encodeURIComponent(state.node)}/assets/${encodeURIComponent(marker.id)}`);
    return `<a class="attthumb legacy-reference-media" data-asset-preview data-name="${esc(name)}" href="${url}" title="${esc(name)}"><img src="${url}" alt="${esc(name)}" loading="lazy"></a>`;
  }).join("");
  return { clean, html:`<div class="attrow reference-media legacy">${tiles}</div>` };
}
