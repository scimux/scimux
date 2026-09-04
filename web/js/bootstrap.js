/* FR-40 remote bootstrap. Checked-in source, no build step.
 *
 * The page is handed a channel fetch (not a peer connection), a computer-
 * supplied manifest, and platform seams for object URLs, import maps and
 * module import. It fetches every named asset, verifies each digest, rewrites
 * module specifiers, installs the import map, and only then starts the
 * entry. A failure of any one asset is a named abort; the entry never runs.
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

function rewriteSpecifiers(source, fromUrl) {
  return source.replace(SPECIFIER_RE, (m, prefix, quote, spec) => {
    return prefix + quote + resolveSpecifier(fromUrl, spec) + quote;
  });
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
    const payload = entry.kind === "js"
      ? rewriteSpecifiers(new TextDecoder().decode(bytes), url)
      : bytes;
    const blobURL = createObjectURL(new Blob([payload], { type: blobType(entry.kind) }));
    blobs.set(url, blobURL);
    imports[url] = blobURL;
    if (entry.kind === "css") stylesheets.push({ url, blobURL });
    if (entry.kind === "asset") assets.push({ url, blobURL });
  }

  // The map keys on rewritten, root-relative URLs and nothing else. A key that
  // is not a bare specifier is parsed as a URL against the map's base — the
  // document — so "./chat.js" would normalise to <origin>/chat.js and never be
  // consulted; rewriteSpecifiers has already made it "/js/chat.js", which does
  // resolve correctly from a blob: module. Relative keys would be dead weight
  // that reads like a working mapping.
  installImportMap({ imports });

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
