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
function activateBrowser(result, { document: doc, window: win, fetchImpl }) {
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
  result.module.createApp({
    fetchImpl,
    assetURL: path => assets.get(path) || "",
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
    });
  }
  return result;
}
