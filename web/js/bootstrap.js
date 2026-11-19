/* FR-40 remote bootstrap. Checked-in source, no build step.
 *
 * The page is handed a channel fetch (not a peer connection), a computer-
 * supplied manifest, and platform seams for object URLs, import maps and
 * module import. It fetches every named asset, verifies each digest, mints one
 * blob: URL per asset, rewrites each module's specifiers to the blob: URLs of
 * the modules it imports, and only then starts the entry. A failure of any one
 * asset is a named abort; the entry never runs.
 *
 * Specifiers are rewritten to absolute blob: URLs rather than to root-relative
 * paths behind an import map, because a blob: URL has an opaque path and
 * nothing relative resolves against one: `new URL("/js/chat.js", blobURL)` is a
 * parse failure in every browser, in Node, and in the URL Standard. An import
 * map cannot rescue that — a URL-like map key normalises against the document,
 * so it can never equal a specifier that failed to normalise at all — and the
 * `installImportMap` seam is therefore accepted and left uncalled. Rewriting to
 * an absolute URL removes the base from the question entirely.
 *
 * Integrity values are accepted only when the manifest names the computer as
 * their source. A rendezvous-offered manifest is refused before the channel
 * is touched.
 *
 * Network access goes through the injected `channel` function. This file
 * must not call global fetch, dynamic import(), or construct a peer
 * connection — those would bypass the seam and the audit.
 */
"use strict";

export const SOURCE_COMPUTER = "computer";
export const SOURCE_RENDEZVOUS = "rendezvous";

const ENTRY = "/js/app.js";
const SPECIFIER_RE = /((?:from|import)\s+)(["'])(\.[^"']+)\2/g;

function named(name, message) {
  const err = new Error(message);
  err.name = name;
  return err;
}

async function sriOf(bytes) {
  const hash = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  let bin = "";
  for (const b of hash) bin += String.fromCharCode(b);
  return "sha256-" + btoa(bin);
}

function resolveSpecifier(fromUrl, spec) {
  if (!spec.startsWith(".")) return spec;
  const slash = fromUrl.lastIndexOf("/");
  const dir = slash < 0 ? "/" : fromUrl.slice(0, slash + 1);
  const parts = (dir + spec).split("/");
  const stack = [];
  for (const p of parts) {
    if (p === "" || p === ".") continue;
    if (p === "..") stack.pop();
    else stack.push(p);
  }
  return "/" + stack.join("/");
}

function rewriteSpecifiers(source, fromUrl, blobs) {
  return source.replace(SPECIFIER_RE, (m, prefix, quote, spec) => {
    const target = blobs.get(resolveSpecifier(fromUrl, spec));
    if (!target) {
      throw named(
        "bootstrap-order",
        fromUrl + " imports " + spec + ", which has no object URL yet",
      );
    }
    return prefix + quote + target + quote;
  });
}

/* Dependencies first. A module's blob can only be minted once every module it
 * imports has one, so the graph is walked depth-first and emitted post-order.
 * The walk is also the cycle check: a blob graph cannot express a cycle — one
 * of the two modules would have to be minted before the other's URL exists —
 * so a cycle is a named abort here rather than a half-built graph later.
 * scimux's graph is acyclic — AT-FR-40-a boots the real one — and AT-FR-40-f
 * pins what a later cycle would do. */
function moduleOrder(bodies) {
  const order = [];
  const state = new Map();
  const path = [];
  const decoder = new TextDecoder();

  const visit = url => {
    const seen = state.get(url);
    if (seen === "done") return;
    if (seen === "open") {
      const cycle = path.slice(path.indexOf(url)).concat(url).join(" -> ");
      throw named("bootstrap-cyclic", "import cycle " + cycle + " cannot be resolved to object URLs");
    }
    state.set(url, "open");
    path.push(url);
    for (const spec of specifiersIn(decoder.decode(bodies.get(url).bytes))) {
      const dep = resolveSpecifier(url, spec);
      const body = bodies.get(dep);
      if (body && body.entry.kind === "js") visit(dep);
    }
    path.pop();
    state.set(url, "done");
    order.push(url);
  };

  for (const [url, { entry }] of bodies) {
    if (entry.kind === "js") visit(url);
  }
  return order;
}

function specifiersIn(source) {
  const out = [];
  const re = /(?:from|import)\s+["'](\.[^"']+)["']/g;
  let m;
  while ((m = re.exec(source))) out.push(m[1]);
  return out;
}

function blobType(kind) {
  if (kind === "js") return "text/javascript";
  if (kind === "css") return "text/css";
  if (kind === "asset") return "image/svg+xml";
  return "application/octet-stream";
}

/* The loader owns the computer document as well as its graph. rv must not
 * learn createApp's API or which verified objects are styles and assets. */
export function createRemoteAssetTransport({ channel, assets, createObjectURL, revokeObjectURL = url => URL.revokeObjectURL(url), peopleByteBudget = 256 * 1024 * 1024 }) {
  const pending = new Map();
  let peopleServers = new Set(), verifiedServers = new Map(), copyInstances = new Map(), capabilityVisit = 0, capabilityPending = null;
  const serverEpoch = new Map();
  const peopleAssets = new Map(), copyEpoch = new Map();
  let peopleBytes = 0;
  async function load(path) {
    // Requests are limited to canonical local assets and exact shared-message
    // associations. The server independently authorizes every association.
    const shared = /^\/api\/team\/subscriptions\/[0-9a-f]{32}\/[0-9a-f]{32}\/[0-9a-f]{16}\/[0-9a-f]{16}\/attachments\/[1-9][0-9]*\/[0-9a-f]{16}\?copy_instance=[0-9a-f]{32}(?:&preview=1)?$/.test(path);
    const nativeAttachment = /^\/api\/nodes\/[^/?#]+\/attachments\/[0-9a-f]{8}-[^/?#]+$/.test(path);
    if ((!shared && !nativeAttachment && !/^\/api\/nodes\/[^/?#]+\/assets\/[^/?#]+$/.test(path)) || assets.has(path)) return;
    const copy = shared ? path.slice(0, path.indexOf('/attachments/')) : '';
    if (shared && copyInstances.get(copy) !== new URLSearchParams(path.split('?')[1]).get('copy_instance')) return;
    const epoch = copyEpoch.get(copy) || 0, server = shared ? copy.split('/')[4] : '', binding = serverEpoch.get(server) || 0;
    if ((shared || nativeAttachment) && (peopleAssets.size >= 4096 || peopleBytes >= peopleByteBudget)) return;
    if (pending.has(path)) return pending.get(path);
    const task = (async () => {
      try {
        const response = await channel(path);
        if (!response.ok) return;
        if (shared && Number(response.headers.get('Content-Length')) > 4194304) return;
        if (nativeAttachment && Number(response.headers.get('Content-Length')) > 25 * 1024 * 1024) return;
        const bytes = await response.arrayBuffer();
        if (shared && (bytes.byteLength < 1 || bytes.byteLength > 4194304)) return;
        if (nativeAttachment && bytes.byteLength > 25 * 1024 * 1024) return;
        const type = response.headers.get('Content-Type') || 'application/octet-stream';
        if (shared && path.endsWith('&preview=1') && type !== 'image/png' && type !== 'image/jpeg') return;
        if (shared && !path.endsWith('&preview=1') && type !== 'application/octet-stream') return;
        if (nativeAttachment && !['application/octet-stream', 'image/png', 'image/jpeg', 'image/gif', 'image/webp'].includes(type)) return;
        if (shared && capabilityPending) {
          let timer, timedOut = false;
          const timeout = new Promise(resolve => { timer = setTimeout(() => { timedOut = true; resolve(); }, 10000); });
          try { while (capabilityPending && !timedOut) await Promise.race([capabilityPending, timeout]); }
          finally { clearTimeout(timer); }
        }
        if (shared && (capabilityPending || !peopleServers.has(server) || binding !== (serverEpoch.get(server) || 0) || epoch !== (copyEpoch.get(copy) || 0) || peopleAssets.size >= 4096 || peopleBytes + bytes.byteLength > peopleByteBudget)) return;
        if (nativeAttachment && (peopleAssets.size >= 4096 || peopleBytes + bytes.byteLength > peopleByteBudget)) return;
        assets.set(path, createObjectURL(new Blob([bytes], { type })));
        if (shared || nativeAttachment) { peopleAssets.set(path, bytes.byteLength); peopleBytes += bytes.byteLength; }
      } catch {
        // A failed image must not hide the chat; the next poll retries it.
      }
    })();
    pending.set(path, task);
    try { await task; } finally { pending.delete(path); }
  }
  async function prepare(value) {
    if (!value || typeof value !== 'object') return;
    if (value.assets && typeof value.assets === 'object') {
      // Sequential loads also stay below the tunnel's concurrent-request cap.
      for (const asset of Object.values(value.assets)) {
        if (asset && typeof asset.url === 'string') await load(asset.url);
      }
    }
    // History segments and bookmark previews may contain their own assets.
    for (const [key, child] of Object.entries(value)) {
      if (key !== 'assets' && child && typeof child === 'object') await prepare(child);
    }
  }
  async function preparePeople(value, requested) {
    if (typeof requested !== 'string') return;
    const match = requested.match(/^(\/api\/team\/subscriptions\/[0-9a-f]{32}\/[0-9a-f]{32}\/[0-9a-f]{16}\/[0-9a-f]{16})\/comments(?:\?[^#]*)?$/);
    if (!match || !peopleServers.has(match[1].split('/')[4]) || !Array.isArray(value?.entries)) return;
    const requestedCopy = new URLSearchParams(requested.split('?')[1] || '').get('copy_instance');
    if (requestedCopy !== copyInstances.get(match[1])) return;
    const copy = match[1], server = copy.split('/')[4];
    const copyVisit = copyEpoch.get(copy), serverVisit = serverEpoch.get(server);
    const unchanged = () => !capabilityPending && peopleServers.has(server) && copyInstances.get(copy) === requestedCopy && copyEpoch.get(copy) === copyVisit && serverEpoch.get(server) === serverVisit;
    async function settleCapabilities() {
      if (capabilityPending) {
        let timer, timedOut = false;
        const timeout = new Promise(resolve => { timer = setTimeout(() => { timedOut = true; resolve(); }, 10000); });
        try { while (capabilityPending && !timedOut) await Promise.race([capabilityPending, timeout]); }
        finally { clearTimeout(timer); }
      }
    }
    let count = 0;
    for (const entry of value.entries) {
      if (!Number.isSafeInteger(entry?.seq) || entry.seq < 1 || !Array.isArray(entry.attachments) || entry.attachments.length > 16) continue;
      for (const file of entry.attachments) {
        if (++count > 256) return;
        if (!file || typeof file.id !== 'string' || !/^[0-9a-f]{16}$/.test(file.id) || !Number.isSafeInteger(file.size) || file.size < 1 || file.size > 4194304) continue;
        await settleCapabilities();
        if (!unchanged()) return;
        const path = copy + '/attachments/' + entry.seq + '/' + file.id + '?copy_instance=' + requestedCopy;
        await load(path);
        await settleCapabilities();
        if (!unchanged()) return;
        if (file.mime === 'image/png' || file.mime === 'image/jpeg') await load(path + '&preview=1');
      }
    }
  }
  async function prepareNativeUploads(value, requested, options) {
    if (typeof requested !== 'string' || options?.method !== 'POST' || !Array.isArray(value?.attachments)) return;
    const match = requested.match(/^(\/api\/nodes\/[^/?#]+\/attachments)(?:\?[^#]*)?$/);
    if (!match || value.attachments.length > 16) return;
    for (const attachment of value.attachments) {
      if (typeof attachment?.path !== 'string') continue;
      const leaf = attachment.path.split('/').pop();
      if (!/^[0-9a-f]{8}-[^/\\\x00-\x1f\x7f]{1,255}$/.test(leaf)) continue;
      await load(match[1] + '/' + encodeURIComponent(leaf));
    }
  }
  function applyCapabilities(value) {
    const limits = value?.people_attachments;
    const valid = limits?.version === 1 && limits.max_file_bytes === 4194304 && limits.max_files === 16 && limits.max_total_bytes === 16777216 && Array.isArray(value?.servers);
    const next = new Map();
    if (valid) for (const server of value.servers) {
      if (server?.people_files === 'supported' && /^[0-9a-f]{32}$/.test(server.id || '')) next.set(server.id, JSON.stringify([server.people_files, server.people_file_binding ?? null]));
    }
    for (const id of new Set([...verifiedServers.keys(), ...next.keys()])) {
      if (verifiedServers.get(id) !== next.get(id)) serverEpoch.set(id, (serverEpoch.get(id) || 0) + 1);
    }
    verifiedServers = next;
    peopleServers = new Set(next.keys());
    const copies = new Map();
    if (valid && Array.isArray(value.subscriptions)) for (const item of value.subscriptions) {
      if (/^[0-9a-f]{32}$/.test(item?.people_copy_instance || '') && /^[0-9a-f]{32}$/.test(item.server || '') && /^[0-9a-f]{32}$/.test(item.owner || '') && /^[0-9a-f]{16}$/.test(item.chat || '') && /^[0-9a-f]{16}$/.test(item.generation || '')) copies.set('/api/team/subscriptions/' + [item.server,item.owner,item.chat,item.generation].join('/'), item.people_copy_instance);
    }
    for (const copy of new Set([...copyInstances.keys(), ...copies.keys()])) if (copyInstances.get(copy) !== copies.get(copy)) {
      copyEpoch.set(copy, (copyEpoch.get(copy) || 0) + 1);
      for (const [path, size] of peopleAssets) if (path.startsWith(copy + '/attachments/')) {
        const url = assets.get(path); if (url) revokeObjectURL(url);
        assets.delete(path); peopleAssets.delete(path); peopleBytes -= size;
      }
    }
    copyInstances = copies;
  }
  return {
    assetURL: path => assets.get(path) || '',
    retryAsset: async path => {
      const match = typeof path === 'string' && path.match(/^\/api\/team\/subscriptions\/([0-9a-f]{32})\/[0-9a-f]{32}\/[0-9a-f]{16}\/[0-9a-f]{16}\/attachments\/[1-9][0-9]*\/[0-9a-f]{16}\?copy_instance=[0-9a-f]{32}$/);
      if (!match || capabilityPending || !peopleServers.has(match[1])) return '';
      await load(path);
      return assets.get(path) || '';
    },
    fetchImpl: async (...args) => {
      const isCapabilities = typeof args[0] === 'string' && args[0].split('?')[0] === '/api/team/subscriptions';
      const deletePath = typeof args[0] === 'string' && args[1]?.method === 'DELETE' ? args[0].split('?')[0] : '';
      const deleteCopy = /^\/api\/team\/subscriptions\/[0-9a-f]{32}\/[0-9a-f]{32}\/[0-9a-f]{16}\/[0-9a-f]{16}$/.test(deletePath);
      const deleteInstance = deleteCopy ? copyInstances.get(deletePath) : undefined;
      const deleteEpoch = deleteCopy ? (copyEpoch.get(deletePath) || 0) : 0;
      const requestedInstance = deleteCopy ? new URLSearchParams(args[0].split('?')[1] || '').get('copy_instance') : null;
      const visit = isCapabilities ? ++capabilityVisit : 0;
      let settle;
      if (isCapabilities) { peopleServers = new Set(); capabilityPending = new Promise(resolve => { settle = resolve; }); }
      let response;
      try { response = await channel(...args); }
      catch (err) { if (isCapabilities) { if (visit === capabilityVisit) { applyCapabilities(null); capabilityPending = null; } settle(); } throw err; }
      if (deleteCopy && response.ok && copyInstances.get(deletePath) === deleteInstance && (copyEpoch.get(deletePath) || 0) === deleteEpoch && (requestedInstance === null || requestedInstance === deleteInstance)) {
        const copy = deletePath; copyEpoch.set(copy, (copyEpoch.get(copy) || 0) + 1);
        for (const [path, size] of peopleAssets) if (path.startsWith(copy + '/attachments/')) {
          const url = assets.get(path); if (url) revokeObjectURL(url);
          assets.delete(path); peopleAssets.delete(path); peopleBytes -= size;
        }
      }
      const json = response.json.bind(response);
      response.json = async () => {
        let value;
        try { value = await json(); }
        catch (err) { if (isCapabilities) { if (visit === capabilityVisit) { applyCapabilities(null); capabilityPending = null; } settle(); } throw err; }
        if (isCapabilities) { if (visit === capabilityVisit) { applyCapabilities(response.ok ? value : null); capabilityPending = null; } settle(); }
        await prepare(value);
        await preparePeople(value, args[0]);
        await prepareNativeUploads(value, args[0], args[1]);
        return value;
      };
      return response;
    },
  };
}

function activateBrowser(result, { document: doc, window: win, fetchImpl, createObjectURL }) {
  if (!result.module || typeof result.module.createApp !== "function") {
    throw named("bootstrap-entry", "the entry module exports no createApp()");
  }
  if (!win || typeof win.DOMParser !== "function" || !doc) {
    throw named("bootstrap-document", "the browser cannot install the application document");
  }

  const parsed = new win.DOMParser().parseFromString(result.index, "text/html");
  if (!parsed || !parsed.head || !parsed.body) {
    throw named("bootstrap-document", "the application index is not HTML");
  }
  for (const script of parsed.querySelectorAll("script")) script.remove();

  const styles = new Map(result.stylesheets.map(row => [row.url, row.blobURL]));
  for (const link of parsed.querySelectorAll('link[rel="stylesheet"]')) {
    const href = link.getAttribute("href");
    const blobURL = styles.get(href);
    if (!blobURL) throw named("bootstrap-missing", "no verified object URL for " + href);
    link.setAttribute("href", blobURL);
  }

  doc.head.replaceChildren(...Array.from(parsed.head.childNodes, node => doc.importNode(node, true)));
  doc.body.replaceChildren(...Array.from(parsed.body.childNodes, node => doc.importNode(node, true)));

  const assets = new Map(result.assets.map(row => [row.url, row.blobURL]));
  const transport = createRemoteAssetTransport({ channel: fetchImpl, assets, createObjectURL });
  result.module.createApp({
    ...transport,
    document: doc,
    window: win,
  });
}

export async function bootstrap({
  channel,
  manifest,
  createObjectURL,
  // Part of the §8 signature and deliberately unused: rv passes it, older
  // loaders called it, and dropping the parameter would be a MAJOR bump for
  // no gain. See the note at the top of this file for why no map is installed.
  installImportMap,
  importModule,
} = {}) {
  if (!manifest || manifest.source !== SOURCE_COMPUTER) {
    throw named(
      "bootstrap-integrity-source",
      "integrity values must come from the computer channel; a rendezvous-offered value is refused",
    );
  }

  const entries = Array.isArray(manifest.entries) ? manifest.entries : [];
  const bodies = new Map();
  for (const entry of entries) {
    const res = await channel(entry.url);
    if (!res || !res.ok) {
      throw named("bootstrap-missing", "channel did not serve " + entry.url);
    }
    const raw = await res.arrayBuffer();
    const bytes = raw instanceof Uint8Array ? raw : new Uint8Array(raw);
    if (typeof entry.size === "number" && bytes.byteLength !== entry.size) {
      throw named("bootstrap-truncated", "channel truncated " + entry.url);
    }
    const got = await sriOf(bytes);
    if (got !== entry.integrity) {
      throw named("bootstrap-integrity", "integrity failed for " + entry.url);
    }
    bodies.set(entry.url, { entry, bytes });
  }

  // Prove the graph is closed before minting anything. A specifier naming a
  // module the manifest omitted passes the fetch-and-verify loop untouched —
  // there is no entry to fail on — and would surface only when the entry
  // evaluated its imports, which is a partial boot arriving late.
  for (const [url, { entry, bytes }] of bodies) {
    if (entry.kind !== "js") continue;
    for (const spec of specifiersIn(new TextDecoder().decode(bytes))) {
      const resolved = resolveSpecifier(url, spec);
      if (!bodies.has(resolved)) {
        throw named(
          "bootstrap-unresolved",
          url + " imports " + spec + ", which the manifest does not name",
        );
      }
    }
  }

  const blobs = new Map();
  const imports = {};
  const stylesheets = [];
  const assets = [];
  let index = "";

  for (const [url, { entry, bytes }] of bodies) {
    if (entry.kind === "index") {
      index = new TextDecoder().decode(bytes);
      continue;
    }
    if (entry.kind === "js") continue; // minted in dependency order below
    const blobURL = createObjectURL(new Blob([bytes], { type: blobType(entry.kind) }));
    blobs.set(url, blobURL);
    imports[url] = blobURL;
    if (entry.kind === "css") stylesheets.push({ url, blobURL });
    if (entry.kind === "asset") assets.push({ url, blobURL });
  }

  for (const url of moduleOrder(bodies)) {
    const source = rewriteSpecifiers(
      new TextDecoder().decode(bodies.get(url).bytes),
      url,
      blobs,
    );
    const blobURL = createObjectURL(new Blob([source], { type: blobType("js") }));
    blobs.set(url, blobURL);
    imports[url] = blobURL;
  }

  const entryUrl = manifest.entry || ENTRY;
  const entryBlob = blobs.get(entryUrl);
  if (!entryBlob) {
    throw named("bootstrap-missing", "manifest entry " + entryUrl + " was not in the graph");
  }
  // app.js self-boots only from an http(s) URL, which the entry blob never is.
  // The computer loader therefore installs and starts it with the channel
  // transport as fetchImpl and verified blob URLs for assetURL. Returning the
  // namespace as well keeps the handover observable without duplicating boot.
  const module = await importModule(entryBlob);

  const result = { index, imports, stylesheets, assets, entry: entryBlob, module };
  if (typeof globalThis.DOMParser === "function") {
    activateBrowser(result, {
      document: globalThis.document,
      window: globalThis.window,
      fetchImpl: channel,
      createObjectURL,
    });
  }
  return result;
}
