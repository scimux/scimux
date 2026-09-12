/* S8-loader R1 — AT-FR-40-a..d against a fake channel.
 *
 * Failures are missing bootstrap behaviour, not load errors in this file.
 * The served set is derived from disk the same way
 * internal/app/remote_served_set_test.go:50 derives it from the embed:
 * index at "/", flat /js/*.js, flat /css/*.css, recursive /assets/**.
 * A hand-maintained filename list is the failure FR-40 names.
 */
import test from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, relative } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

import {
  bootstrap,
  SOURCE_COMPUTER,
  SOURCE_RENDEZVOUS,
} from "../js/bootstrap.js";

const here = dirname(fileURLToPath(import.meta.url));
const WEB = join(here, "..");
const ENTRY = "/js/app.js";

const SPECIFIER_RE = /(?:from|import)\s+["'](\.[^"']+)["']/g;

function fail(at, detail) {
  assert.fail(`${at}: ${detail}`);
}

/* Mirror of servedAssetInventory. Directories under js/ or css/ are a
 * packaging error there; here they are skipped the same way a handler
 * would 404 them — the walk is files only. */
function servedFromDisk() {
  const out = [{ url: "/", file: join(WEB, "index.html"), kind: "index" }];

  for (const { dir, prefix, ext, kind } of [
    { dir: join(WEB, "js"), prefix: "/js/", ext: ".js", kind: "js" },
    { dir: join(WEB, "css"), prefix: "/css/", ext: ".css", kind: "css" },
  ]) {
    for (const name of readdirSync(dir).sort()) {
      const file = join(dir, name);
      if (statSync(file).isDirectory()) continue;
      if (!name.endsWith(ext)) continue;
      out.push({ url: prefix + name, file, kind });
    }
  }

  function walk(dir) {
    for (const name of readdirSync(dir).sort()) {
      const file = join(dir, name);
      if (statSync(file).isDirectory()) {
        walk(file);
        continue;
      }
      const rel = relative(join(WEB, "assets"), file).split("\\").join("/");
      out.push({ url: "/assets/" + rel, file, kind: "asset" });
    }
  }
  walk(join(WEB, "assets"));

  out.sort((a, b) => (a.url < b.url ? -1 : a.url > b.url ? 1 : 0));
  return out;
}

async function sriOf(bytes) {
  const hash = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  let bin = "";
  for (const b of hash) bin += String.fromCharCode(b);
  return "sha256-" + btoa(bin);
}

async function computerManifest(inventory) {
  const entries = [];
  for (const item of inventory) {
    const bytes = new Uint8Array(readFileSync(item.file));
    entries.push({
      url: item.url,
      kind: item.kind,
      size: bytes.byteLength,
      integrity: await sriOf(bytes),
    });
  }
  return { source: SOURCE_COMPUTER, entry: ENTRY, entries };
}

function bytesResponse(bytes, status = 200) {
  const copy = Uint8Array.from(bytes);
  return {
    ok: status >= 200 && status < 300,
    status,
    async arrayBuffer() {
      return copy.buffer.slice(copy.byteOffset, copy.byteOffset + copy.byteLength);
    },
    async text() {
      return new TextDecoder().decode(copy);
    },
  };
}

function diskChannel(inventory, { omit, truncate, tamper } = {}) {
  const byUrl = new Map(inventory.map(i => [i.url, i]));
  const calls = [];
  const fetchImpl = async url => {
    const path = String(url).split("?")[0];
    calls.push(path);
    if (omit && omit.has(path)) {
      return bytesResponse(new Uint8Array(), 404);
    }
    const item = byUrl.get(path);
    if (!item) return bytesResponse(new Uint8Array(), 404);
    let bytes = new Uint8Array(readFileSync(item.file));
    if (truncate && truncate.has(path) && bytes.byteLength > 0) {
      bytes = bytes.slice(0, bytes.byteLength - 1);
    }
    if (tamper && tamper.has(path) && bytes.byteLength > 0) {
      bytes = Uint8Array.from(bytes);
      bytes[0] ^= 0xff;
    }
    return bytesResponse(bytes);
  };
  fetchImpl.calls = calls;
  return fetchImpl;
}

function seams() {
  const blobs = new Map();
  let n = 0;
  const imported = [];
  const maps = [];
  return {
    createObjectURL(blob) {
      n += 1;
      const url = `blob:boot/${n}`;
      blobs.set(url, blob);
      return url;
    },
    installImportMap(map) {
      maps.push(map);
    },
    async importModule(url) {
      imported.push(url);
      return { url };
    },
    blobs,
    imported,
    maps,
  };
}

function specifiersIn(source) {
  const out = [];
  SPECIFIER_RE.lastIndex = 0;
  let m;
  while ((m = SPECIFIER_RE.exec(source))) out.push(m[1]);
  return out;
}

function resolveSpecifier(fromUrl, spec) {
  if (!spec.startsWith(".")) return spec;
  const slash = fromUrl.lastIndexOf("/");
  const dir = slash < 0 ? "" : fromUrl.slice(0, slash + 1);
  const parts = (dir + spec).split("/");
  const stack = [];
  for (const p of parts) {
    if (p === "" || p === ".") continue;
    if (p === "..") stack.pop();
    else stack.push(p);
  }
  return "/" + stack.join("/");
}

test("AT-FR-40-a: fake channel serving the real files boots the full graph", async () => {
  const at = "AT-FR-40-a";
  const inventory = servedFromDisk();
  const manifest = await computerManifest(inventory);
  const channel = diskChannel(inventory);
  const s = seams();

  let result;
  try {
    result = await bootstrap({
      channel,
      manifest,
      createObjectURL: s.createObjectURL,
      installImportMap: s.installImportMap,
      importModule: s.importModule,
    });
  } catch (err) {
    fail(at, `bootstrap threw: ${err && err.message ? err.message : err}`);
  }

  const js = inventory.filter(i => i.kind === "js");
  const css = inventory.filter(i => i.kind === "css");
  const assets = inventory.filter(i => i.kind === "asset");
  const fetched = new Set(channel.calls);

  for (const item of inventory) {
    if (!fetched.has(item.url)) {
      fail(at, `channel never fetched ${item.url}`);
    }
  }
  if (css.length !== 10) {
    fail(at, `disk has ${css.length} stylesheets, want 10`);
  }
  if (assets.length !== 6) {
    fail(at, `disk has ${assets.length} assets, want 6`);
  }
  if (!s.imported.length) {
    fail(at, "entry module was not started");
  }
  if (s.maps.length !== 0) {
    fail(at, "an import map was installed; a blob: module resolves no URL-like " +
      "specifier at all, so the map is unreachable by construction — see AT-FR-40-e");
  }

  // Every module the loader must resolve, as the *resolved* URL, each carrying
  // its own object URL. The graph is closed, so the set of resolved specifiers
  // is the set of module URLs; asserting both sides keeps a loader that mints
  // blobs but drops an edge from passing.
  const graphSpecs = new Set();
  for (const item of js) {
    graphSpecs.add(item.url);
    const src = readFileSync(item.file, "utf8");
    for (const spec of specifiersIn(src)) {
      graphSpecs.add(resolveSpecifier(item.url, spec));
    }
  }
  const mapped = result.imports;
  for (const spec of graphSpecs) {
    if (!mapped[spec]) {
      fail(at, `no object URL was minted for ${spec}`);
    }
  }
  for (const [url, blobURL] of Object.entries(mapped)) {
    if (!String(blobURL).startsWith("blob:")) {
      fail(at, `${url} resolves to ${blobURL}, which is not an object URL`);
    }
  }
  // One object URL per served asset but the index, so a module is a single
  // instance in the browser's module map rather than one copy per importer.
  if (Object.keys(mapped).length !== inventory.length - 1) {
    fail(at, `${Object.keys(mapped).length} object URLs for ${inventory.length - 1} ` +
      `non-index assets; a module minted twice is two module instances`);
  }

  const indexOnDisk = readFileSync(join(WEB, "index.html"), "utf8");
  if (result.index !== indexOnDisk) {
    fail(at, "index.html was not returned as verified text");
  }
  if (s.imported[0] !== mapped[ENTRY]) {
    fail(at, `entry module started as ${s.imported[0]}, want the blob for ${ENTRY}`);
  }
});

/* The property the Node seams cannot show by running the graph, and the one
 * that kept the real device on the failure screen: a blob: URL has an opaque
 * path, so `new URL("/js/chat.js", blobURL)` is a parse failure and a
 * specifier that fails to parse never reaches an import-map key at all. The
 * map was not late, it was unreachable. Every specifier that survives into a
 * minted module must therefore be an absolute blob: URL, and no map may be
 * installed to paper over one that is not. */
test("AT-FR-40-e: every verified module import is an absolute blob: URL, with no import map", async () => {
  const at = "AT-FR-40-e";
  const inventory = servedFromDisk();
  const manifest = await computerManifest(inventory);
  const s = seams();
  const result = await bootstrap({
    channel: diskChannel(inventory),
    manifest,
    createObjectURL: s.createObjectURL,
    installImportMap() {
      throw new Error("an import map is unreachable from a blob: module and must not be installed");
    },
    importModule: s.importModule,
  });

  // The edges the graph actually has, counted off disk with the loader's own
  // notion of a specifier, so the rewritten side can be compared to a number
  // nothing in the loader produced.
  const js = inventory.filter(i => i.kind === "js");
  let edges = 0;
  for (const item of js) edges += specifiersIn(readFileSync(item.file, "utf8")).length;
  if (edges < 20) {
    fail(at, `disk has ${edges} module edges; the check is vacuous`);
  }

  let checked = 0;
  let rewritten = 0;
  for (const blobURL of Object.values(result.imports)) {
    const blob = s.blobs.get(blobURL);
    if (!blob || blob.type !== "text/javascript") continue;
    checked += 1;
    const source = await blob.text();
    const leftover = source.match(/(?:from|import)\s+["'][./][^"']*["']/);
    if (leftover) {
      fail(at, `a minted module still carries ${leftover[0]}; nothing relative or ` +
        `root-absolute resolves against a blob: base`);
    }
    for (const [, spec] of source.matchAll(/(?:from|import)\s+["'](blob:[^"']+)["']/g)) {
      rewritten += 1;
      if (!s.blobs.has(spec)) {
        fail(at, `a minted module imports ${spec}, which is not one of the verified objects`);
      }
    }
  }
  if (checked !== js.length) {
    fail(at, `checked ${checked} modules, disk has ${js.length}`);
  }
  if (rewritten !== edges) {
    fail(at, `${rewritten} specifiers were rewritten to object URLs, disk has ${edges} edges`);
  }
});

/* A blob graph cannot express a cycle: whichever module is minted first would
 * need the other's object URL, which does not exist yet. scimux's graph is
 * acyclic — AT-FR-40-a boots the real one — and this names what happens if a
 * later change makes it cyclic: a clear abort here, not a partial boot on a
 * device. */
test("AT-FR-40-f: an import cycle is a named abort and never starts the entry", async () => {
  const at = "AT-FR-40-f";
  // The two fixture bodies are assembled rather than written out literally:
  // reachability.test.js scans this file's source for import statements naming
  // a relative module, and would read a fixture body as one of them.
  const imports = (what, name) => "import { " + what + " } from " + JSON.stringify("./" + name) + ";\n";
  const files = new Map([
    ["/", "<!doctype html><html><head></head><body></body></html>"],
    ["/js/app.js", imports("b", "b.js") + "export function createApp() { return b; }\n"],
    ["/js/b.js", imports("createApp", "app.js") + "export const b = createApp;\n"],
  ]);
  const kinds = new Map([["/", "index"], ["/js/app.js", "js"], ["/js/b.js", "js"]]);

  const entries = [];
  for (const [url, text] of files) {
    const bytes = new TextEncoder().encode(text);
    entries.push({ url, kind: kinds.get(url), size: bytes.byteLength, integrity: await sriOf(bytes) });
  }
  const s = seams();
  await assert.rejects(
    bootstrap({
      channel: async url => bytesResponse(new TextEncoder().encode(files.get(url) ?? "")),
      manifest: { source: SOURCE_COMPUTER, entry: ENTRY, entries },
      createObjectURL: s.createObjectURL,
      installImportMap: s.installImportMap,
      importModule: s.importModule,
    }),
    err => {
      assert.equal(err.name, "bootstrap-cyclic");
      assert.match(err.message, /\/js\/app\.js|\/js\/b\.js/);
      return true;
    },
  );
  if (s.imported.length !== 0) {
    fail(at, "the entry was started despite the cycle");
  }
});

// A manifest that omits a module another module imports is the partial boot
// FR-40 forbids, arriving by a different door: every named asset fetches and
// verifies, so the integrity loop is satisfied, and the gap only becomes an
// error later when the entry evaluates its imports. The loader has the graph
// in hand and must refuse before it starts anything.
test("AT-FR-40-b: a specifier resolving outside the manifest aborts and never starts the entry", async () => {
  const at = "AT-FR-40-b";
  const inventory = servedFromDisk();

  // A module some other module imports, and not the entry itself.
  const imported = new Set();
  for (const item of inventory.filter(i => i.kind === "js")) {
    const src = readFileSync(item.file, "utf8");
    for (const spec of specifiersIn(src)) imported.add(resolveSpecifier(item.url, spec));
  }
  const dropped = inventory.find(i => i.kind === "js" && i.url !== ENTRY && imported.has(i.url));
  if (!dropped) fail(at, "no module in web/js/ is imported by another; the fixture is wrong");

  // The channel still serves it. Only the manifest forgets it, so nothing in
  // the fetch-and-verify loop can notice.
  const short = inventory.filter(i => i.url !== dropped.url);
  const s = seams();
  await assert.rejects(
    async () => bootstrap({
      channel: diskChannel(inventory),
      manifest: await computerManifest(short),
      createObjectURL: s.createObjectURL,
      installImportMap: s.installImportMap,
      importModule: s.importModule,
    }),
    err => {
      if (!err || !err.name) fail(at, `omitting ${dropped.url} did not abort with a named error`);
      return true;
    },
  );
  if (s.imported.length !== 0) {
    fail(at, `entry module ran despite ${dropped.url} being absent from the manifest`);
  }
});

test("AT-FR-40-b: a missing manifest asset aborts and never starts the entry", async () => {
  const at = "AT-FR-40-b";
  const inventory = servedFromDisk();
  const missing = inventory.find(i => i.kind === "js" && i.url !== ENTRY);
  const channel = diskChannel(inventory, { omit: new Set([missing.url]) });
  const s = seams();
  await assert.rejects(
    async () => bootstrap({
      channel,
      manifest: await computerManifest(inventory),
      createObjectURL: s.createObjectURL,
      installImportMap: s.installImportMap,
      importModule: s.importModule,
    }),
    err => {
      if (!err || !err.name) fail(at, "missing asset did not abort with a named error");
      return true;
    },
  );
  if (s.imported.length) {
    fail(at, `entry module ran after a missing asset (${s.imported}); this is a partial boot`);
  }
});

test("AT-FR-40-b: a truncated asset aborts and never starts the entry", async () => {
  const at = "AT-FR-40-b";
  const inventory = servedFromDisk();
  const target = inventory.find(i => i.kind === "css");
  const channel = diskChannel(inventory, { truncate: new Set([target.url]) });
  const s = seams();
  await assert.rejects(
    async () => bootstrap({
      channel,
      manifest: await computerManifest(inventory),
      createObjectURL: s.createObjectURL,
      installImportMap: s.installImportMap,
      importModule: s.importModule,
    }),
    err => {
      if (!err || !err.name) fail(at, "truncated asset did not abort with a named error");
      return true;
    },
  );
  if (s.imported.length) {
    fail(at, `entry module ran after a truncated asset (${s.imported}); this is a partial boot`);
  }
});

test("AT-FR-40-b: bytes that fail integrity abort and never start the entry", async () => {
  const at = "AT-FR-40-b";
  const inventory = servedFromDisk();
  const target = inventory.find(i => i.kind === "js" && i.url !== ENTRY);
  const channel = diskChannel(inventory, { tamper: new Set([target.url]) });
  const s = seams();
  await assert.rejects(
    async () => bootstrap({
      channel,
      manifest: await computerManifest(inventory),
      createObjectURL: s.createObjectURL,
      installImportMap: s.installImportMap,
      importModule: s.importModule,
    }),
    err => {
      if (!err || !err.name) fail(at, "integrity failure did not abort with a named error");
      return true;
    },
  );
  if (s.imported.length) {
    fail(at, `entry module ran after an integrity failure (${s.imported}); this is a partial boot`);
  }
});

test("AT-FR-40-c: integrity offered by the rendezvous is refused", async () => {
  const at = "AT-FR-40-c";
  const inventory = servedFromDisk();
  const computer = await computerManifest(inventory);
  const rendezvous = { ...computer, source: SOURCE_RENDEZVOUS };
  const channel = diskChannel(inventory);
  const s = seams();
  await assert.rejects(
    async () => bootstrap({
      channel,
      manifest: rendezvous,
      createObjectURL: s.createObjectURL,
      installImportMap: s.installImportMap,
      importModule: s.importModule,
    }),
    err => {
      if (!err || !err.name) fail(at, "rendezvous integrity was not refused with a named error");
      return true;
    },
  );
  if (s.imported.length) {
    fail(at, "entry module ran on rendezvous-offered integrity");
  }
  if (channel.calls.length) {
    fail(at, "a rendezvous-offered manifest must be refused before the channel is used");
  }
});

test("AT-FR-40-d: the suite runs against the repository files and the manifest covers the disk", async () => {
  const at = "AT-FR-40-d";
  const inventory = servedFromDisk();
  const urls = inventory.map(i => i.url);

  if (!urls.includes("/")) fail(at, "inventory is missing /");
  if (!urls.includes(ENTRY)) fail(at, `inventory is missing entry ${ENTRY}`);

  const jsOnDisk = readdirSync(join(WEB, "js")).filter(n => n.endsWith(".js")).sort();
  const cssOnDisk = readdirSync(join(WEB, "css")).filter(n => n.endsWith(".css")).sort();
  const jsUrls = inventory.filter(i => i.kind === "js").map(i => i.url.replace("/js/", "")).sort();
  const cssUrls = inventory.filter(i => i.kind === "css").map(i => i.url.replace("/css/", "")).sort();
  if (jsUrls.join(",") !== jsOnDisk.join(",")) {
    fail(at, `manifest js ${jsUrls} does not cover disk ${jsOnDisk}`);
  }
  if (cssUrls.join(",") !== cssOnDisk.join(",")) {
    fail(at, `manifest css ${cssUrls} does not cover disk ${cssOnDisk}`);
  }
  const assetUrls = inventory.filter(i => i.kind === "asset").map(i => i.url);
  if (!assetUrls.some(u => u.startsWith("/assets/agents/") && u.endsWith(".svg"))) {
    fail(at, "inventory did not recurse into /assets/agents");
  }

  const manifest = await computerManifest(inventory);
  const channel = diskChannel(inventory);
  const s = seams();
  await bootstrap({
    channel,
    manifest,
    createObjectURL: s.createObjectURL,
    installImportMap: s.installImportMap,
    importModule: s.importModule,
  });
  for (const url of urls) {
    if (!channel.calls.includes(url)) {
      fail(at, `boot skipped ${url}, which is on disk; a newly added file would drop out silently`);
    }
  }
});

/* AT-FR-15a-b — the bootstrap half of "no service worker on any origin, in
 * any mode".
 *
 * AT-FR-15a-a (web/test/at-fr15.test.js) already proves this for the page:
 * it scans every served module statically and drives app.js under a
 * register trap on both a localhost and a channel transport. What it cannot
 * reach is the S8 entry path, which did not exist when it was written. On a
 * remote origin the bootstrap is the FIRST code that runs, before app.js is
 * so much as minted, so a registration there would happen entirely outside
 * that test's dynamic half.
 *
 * Two assertions, deliberately of different kinds:
 *
 *   1. The bootstrap never reads `navigator` at all. Trapping only
 *      `serviceWorker.register` would pass a bootstrap that stashed
 *      `navigator.serviceWorker` for later, or reached CacheStorage instead;
 *      asserting the whole object is untouched says the injected seams are
 *      the only platform this function has. It is also why the trap records
 *      reads rather than throwing on them — a throw proves nothing about a
 *      property nobody asked for.
 *
 *   2. Every module the bootstrap actually mints a blob for is free of
 *      service-worker and cache-storage references. This is keyed on the
 *      manifest graph, not on a directory listing: the manifest is FR-40's
 *      authority on what gets executed, so a module entering the graph from
 *      anywhere else is still scanned here.
 *
 * The origin is varied across a localhost and a remote https host to make the
 * "any origin" half literal, even though the bootstrap should be — and is —
 * indifferent to it. If it ever stops being indifferent, that is the finding.
 */

const SERVICE_WORKER_RE = /\bserviceWorker\b|\bServiceWorkerRegistration\b|\bcaches\s*\.\s*(?:open|match|keys|has|delete)\b/;

function installNavigatorTrap() {
  const reads = [];
  const swCalls = [];
  const serviceWorker = new Proxy({
    register(scriptURL, options) {
      swCalls.push({ scriptURL, options });
      return Promise.resolve({});
    },
  }, {
    get(target, prop) {
      reads.push("navigator.serviceWorker." + String(prop));
      return Reflect.get(target, prop);
    },
  });

  const navigator = new Proxy({ serviceWorker, userAgent: "at-fr-15a-b" }, {
    get(target, prop) {
      reads.push("navigator." + String(prop));
      return Reflect.get(target, prop);
    },
  });

  return { reads, swCalls, navigator };
}

/* Async by necessity, not by style. A synchronous version restores the real
 * globals in `finally` as soon as fn() hands back its promise — that is,
 * before the bootstrap's first await has even resumed — so the trap would be
 * uninstalled for the entire run and the assertions on it would be vacuous.
 * This suite caught exactly that: an obfuscated `navigator["service"+"Worker"]`
 * read passed. Await the work inside the swap. */
async function withGlobals(values, fn) {
  const saved = new Map();
  for (const [name, value] of Object.entries(values)) {
    saved.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, {
      configurable: true,
      writable: true,
      value,
    });
  }
  try {
    return await fn();
  } finally {
    for (const [name, desc] of saved) {
      if (desc) Object.defineProperty(globalThis, name, desc);
      else delete globalThis[name];
    }
  }
}

test("AT-FR-15a-b: the FR-40 bootstrap registers no service worker, on any origin, in any mode", async () => {
  const at = "AT-FR-15a-b";
  const inventory = servedFromDisk();
  const manifest = await computerManifest(inventory);

  for (const origin of ["http://127.0.0.1:8787", "https://device.example"]) {
    const trap = installNavigatorTrap();
    const channel = diskChannel(inventory);
    const s = seams();

    const result = await withGlobals({
      navigator: trap.navigator,
      location: { href: origin + "/", origin },
    }, () => bootstrap({
      channel,
      manifest,
      createObjectURL: s.createObjectURL,
      installImportMap: s.installImportMap,
      importModule: s.importModule,
    }));

    // Anti-vacuity: a bootstrap that aborted early would touch nothing and
    // pass every assertion below without having proved anything.
    if (s.imported.length !== 1) {
      fail(at, `boot on ${origin} imported ${s.imported.length} entries, want 1 — the run below proves nothing`);
    }
    if (!result || !result.entry) {
      fail(at, `boot on ${origin} returned no entry blob`);
    }

    if (trap.swCalls.length) {
      fail(at, `serviceWorker.register was called on ${origin}: ${JSON.stringify(trap.swCalls)}`);
    }
    if (trap.reads.length) {
      fail(at, `boot on ${origin} read the platform navigator (${trap.reads.join(", ")}); ` +
        "the injected channel/blob/import-map seams are the only platform the bootstrap may use");
    }
  }

  // The graph the bootstrap actually executes, keyed on the manifest.
  const byUrl = new Map(inventory.map(i => [i.url, i]));
  let scanned = 0;
  for (const entry of manifest.entries) {
    if (entry.kind !== "js" && entry.kind !== "index") continue;
    const item = byUrl.get(entry.url);
    if (!item) fail(at, `manifest names ${entry.url}, which is not on disk`);
    const src = readFileSync(item.file, "utf8");
    scanned += 1;
    if (SERVICE_WORKER_RE.test(src)) {
      fail(at, `${entry.url} is in the boot graph and references a service worker or cache storage`);
    }
  }
  if (scanned < 2) {
    fail(at, `scanned ${scanned} executable graph members; the manifest scan is vacuous`);
  }
});

function browserDocumentFixture() {
  let scriptsRemoved = 0;
  const script = { remove() { scriptsRemoved++; } };
  const link = {
    href: "/css/tokens.css",
    getAttribute(name) { return name === "href" ? this.href : null; },
    setAttribute(name, value) { if (name === "href") this.href = value; },
  };
  const parsed = {
    head: { childNodes: [link] },
    body: { childNodes: [{ id: "app" }] },
    querySelectorAll(selector) {
      if (selector === "script") return [script];
      if (selector === 'link[rel="stylesheet"]') return [link];
      return [];
    },
  };
  const installed = { head: [], body: [] };
  const document = {
    head: { replaceChildren(...nodes) { installed.head = nodes; } },
    body: { replaceChildren(...nodes) { installed.body = nodes; } },
    importNode(node) { return node; },
  };
  const window = { DOMParser: class { parseFromString() { return parsed; } } };
  return { document, window, installed, link, scriptsRemoved: () => scriptsRemoved };
}

/* FR-41 — the boot has to be able to start the app.
 *
 * app.js self-boots only when its own URL is http(s) (web/js/app.js:1878),
 * which is right for the localhost page and impossible remotely: the entry
 * runs from a blob: URL, so the guard never matches. Nothing else calls
 * createApp. So a remote boot that verified and blobbed all 24 modules
 * perfectly still ends at a blank page.
 *
 * The computer loader owns installation as well as verification: rv must not
 * learn the computer entry API or its stylesheet/asset conventions.
 */
test("AT-FR-41-d: the computer loader installs and starts its verified application", async () => {
  const at = "AT-FR-41-d";
  const inventory = servedFromDisk();
  const manifest = await computerManifest(inventory);
  const channel = diskChannel(inventory);
  const s = seams();

  const dom = browserDocumentFixture();
  let appDeps = null;
  const marker = { createApp: deps => { appDeps = deps; } };
  const result = await withGlobals({
    document: dom.document,
    window: dom.window,
    DOMParser: dom.window.DOMParser,
  }, () =>
    bootstrap({
      channel,
      manifest,
      createObjectURL: s.createObjectURL,
      installImportMap: s.installImportMap,
      /* Stand in for a real dynamic import of the entry blob. */
      importModule: async url => (url === undefined ? undefined : marker),
    }));

  if (!result.module) {
    fail(at, "bootstrap returned no entry module; nothing can call createApp, so a remote boot renders nothing");
  }
  if (result.module !== marker) {
    fail(at, "bootstrap returned something other than the imported entry namespace");
  }
  if (typeof result.module.createApp !== "function") {
    fail(at, "the returned namespace is not the entry module");
  }
  assert.equal(dom.scriptsRemoved(), 1, "the localhost script must not run a second time");
  assert.match(dom.link.href, /^blob:boot\//);
  assert.equal(dom.installed.head.length, 1);
  assert.equal(dom.installed.body.length, 1);
  assert.equal(appDeps.document, dom.document);
  assert.equal(appDeps.window, dom.window);
  assert.equal(appDeps.fetchImpl, channel);
  assert.match(appDeps.assetURL("/assets/agents/claude.svg"), /^blob:boot\//);
  assert.equal(appDeps.assetURL("/assets/missing.svg"), "");
  /* Anti-vacuity: the existing return contract is unchanged. */
  for (const key of ["index", "stylesheets", "assets", "entry"]) {
    if (!(key in result)) fail(at, `bootstrap stopped returning ${key}`);
  }
});

/* The check no seam can fake: hand the minted graph to a real module loader
 * and make it resolve and evaluate. file: stands in for blob: because Node has
 * no blob: URLs, and it is a fair stand-in for exactly the property at issue —
 * an absolute specifier is resolved without consulting a base, so a graph that
 * links here is a graph that links from blob: URLs too. Run against the loader
 * as it was before object-URL rewriting, this fails with
 * ERR_MODULE_NOT_FOUND for file:///js/format.js: the root-relative specifier
 * resolved against the module's own base, and the import map — which the
 * browser would never have consulted either — was not there to catch it.
 *
 * It is also the only test that evaluates every module: a module that throws
 * at module scope in the browser cannot pass here. */
test("AT-FR-40-g: the minted graph resolves and evaluates in a real module loader", async t => {
  const at = "AT-FR-40-g";
  const dir = mkdtempSync(join(tmpdir(), "scimux-bootstrap-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));

  const inventory = servedFromDisk();
  const manifest = await computerManifest(inventory);
  const written = [];
  let n = 0;

  const result = await bootstrap({
    channel: diskChannel(inventory),
    manifest,
    // The file is written after bootstrap returns: nothing reads an object URL
    // until the entry is imported, and a Blob's bytes are only available async.
    createObjectURL: blob => {
      const file = join(dir, "m" + ++n + (blob.type === "text/javascript" ? ".mjs" : ".bin"));
      written.push([file, blob]);
      return pathToFileURL(file).href;
    },
    installImportMap() {
      throw new Error("no import map may be installed");
    },
    importModule: async url => url,
  });

  for (const [file, blob] of written) {
    writeFileSync(file, Buffer.from(await blob.arrayBuffer()));
  }
  if (written.length !== inventory.length - 1) {
    fail(at, `minted ${written.length} objects for ${inventory.length - 1} non-index assets`);
  }

  const mod = await import(result.entry);
  if (typeof mod.createApp !== "function") {
    fail(at, "the graph linked but the entry exports no createApp()");
  }
});
