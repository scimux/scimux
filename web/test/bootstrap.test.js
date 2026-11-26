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
  createRemoteAssetTransport,
  SOURCE_COMPUTER,
  SOURCE_RENDEZVOUS,
} from "../js/bootstrap.js";

test("remote transport prepares scoped People attachment preview and download URLs", async()=>{
 const server="a".repeat(32),owner="b".repeat(32),chat="c".repeat(16),generation="d".repeat(16),id="0123456789abcdef";
 const base=`/api/team/subscriptions/${server}/${owner}/${chat}/${generation}`;const calls=[];const assets=new Map();let n=0;
 const channel=async path=>{calls.push(path);if(path==="/api/team/subscriptions")return new Response(JSON.stringify({people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:"supported"}],subscriptions:[{server,owner,chat,generation,people_copy_instance:"f".repeat(32)}]}));if(path.split("?")[0]===base+"/comments")return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id,sha256:"e".repeat(64),size:2,name:"x.png",mime:"image/png"}]}]}),{headers:{"Content-Type":"application/json"}});return new Response(new Uint8Array([1,2]),{headers:{"Content-Type":path.endsWith("&preview=1")?"image/png":"application/octet-stream"}})};
 const transport=createRemoteAssetTransport({channel,assets,createObjectURL:()=>`blob:synthetic-${++n}`});
 await (await transport.fetchImpl(base+"/comments?after=0&copy_instance="+"f".repeat(32))).json();assert.equal(calls.length,1,"no file request before capability discovery");
 await (await transport.fetchImpl("/api/team/subscriptions")).json();
 await (await transport.fetchImpl(base+"/comments?after=0&copy_instance="+"f".repeat(32))).json();
 assert.match(transport.assetURL(base+`/attachments/1/${id}?copy_instance=${"f".repeat(32)}`),/^blob:/);assert.match(transport.assetURL(base+`/attachments/1/${id}?copy_instance=${"f".repeat(32)}&preview=1`),/^blob:/);assert.equal(calls.length,5);
});

test("remote People capability refresh fails closed while a newer result is pending", async()=>{
 const server="a".repeat(32),owner="b".repeat(32),chat="c".repeat(16),generation="d".repeat(16),id="0123456789abcdef";
 const base=`/api/team/subscriptions/${server}/${owner}/${chat}/${generation}`;let release,lists=0;const calls=[];
 const channel=async path=>{calls.push(path);if(path==="/api/team/subscriptions"){lists++;if(lists===2)return new Promise(r=>{release=r});return new Response(JSON.stringify({people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:"supported"}],subscriptions:[{server,owner,chat,generation,people_copy_instance:"f".repeat(32)}]}));}if(path.split("?")[0]===base+"/comments")return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id,size:1,mime:"text/plain"}]}]}));return new Response(new Uint8Array([1]),{headers:{"Content-Type":"application/octet-stream"}})};
 const transport=createRemoteAssetTransport({channel,assets:new Map(),createObjectURL:()=>"blob:synthetic"});
 await (await transport.fetchImpl("/api/team/subscriptions")).json();const refresh=transport.fetchImpl("/api/team/subscriptions");
 await (await transport.fetchImpl(base+"/comments?copy_instance="+"f".repeat(32))).json();assert.equal(calls.length,3,"must not fetch while capability is being refreshed");
 release(new Response(JSON.stringify({servers:[]})));await (await refresh).json();
});

test("remote retained attachment cache removes both copies on explicit deletion", async()=>{
 const server="a".repeat(32),owner="b".repeat(32),chat="c".repeat(16),generation="d".repeat(16),id="0123456789abcdef";
 const base=`/api/team/subscriptions/${server}/${owner}/${chat}/${generation}`;const revoked=[];const assets=new Map();
 const channel=async(path,opts)=>{if(path==="/api/team/subscriptions")return new Response(JSON.stringify({people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:"supported"}],subscriptions:[{server,owner,chat,generation,people_copy_instance:"f".repeat(32)}]}));if(path.split("?")[0]===base+"/comments")return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id,size:2,mime:"image/png"}]}]}));if(opts?.method==="DELETE")return new Response(null,{status:204});return new Response(new Uint8Array([1,2]),{headers:{"Content-Type":path.endsWith("&preview=1")?"image/png":"application/octet-stream"}})};
 const transport=createRemoteAssetTransport({channel,assets,createObjectURL:()=>"blob:synthetic",revokeObjectURL:url=>revoked.push(url),peopleByteBudget:4});
 await (await transport.fetchImpl("/api/team/subscriptions")).json();await (await transport.fetchImpl(base+"/comments?copy_instance="+"f".repeat(32))).json();
 assert.equal(transport.assetURL(base+`/attachments/1/${id}?copy_instance=${"f".repeat(32)}`),"blob:synthetic");assert.equal(transport.assetURL(base+`/attachments/1/${id}?copy_instance=${"f".repeat(32)}&preview=1`),"blob:synthetic");
 await transport.fetchImpl(base,{method:"DELETE"});assert.equal(transport.assetURL(base+`/attachments/1/${id}?copy_instance=${"f".repeat(32)}`),"");assert.equal(transport.assetURL(base+`/attachments/1/${id}?copy_instance=${"f".repeat(32)}&preview=1`),"");assert.deepEqual(revoked,["blob:synthetic","blob:synthetic"]);
});

test("a delayed remote attachment cannot restore a deleted copy cache", async()=>{
 const server="a".repeat(32),owner="b".repeat(32),chat="c".repeat(16),generation="d".repeat(16),id="0123456789abcdef";
 const base=`/api/team/subscriptions/${server}/${owner}/${chat}/${generation}`;let release,entered;const reached=new Promise(r=>{entered=r});
 const channel=async(path,opts)=>{if(path==="/api/team/subscriptions")return new Response(JSON.stringify({people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:"supported"}],subscriptions:[{server,owner,chat,generation,people_copy_instance:"f".repeat(32)}]}));if(path.split("?")[0]===base+"/comments")return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id,size:1,mime:"text/plain"}]}]}));if(opts?.method==="DELETE")return new Response(null,{status:204});entered();return new Promise(r=>{release=r})};
 const transport=createRemoteAssetTransport({channel,assets:new Map(),createObjectURL:()=>"blob:synthetic"});await (await transport.fetchImpl("/api/team/subscriptions")).json();
 const reading=(await transport.fetchImpl(base+"/comments?copy_instance="+"f".repeat(32))).json();await reached;await transport.fetchImpl(base,{method:"DELETE"});release(new Response(new Uint8Array([1]),{headers:{"Content-Type":"application/octet-stream"}}));await reading;
 assert.equal(transport.assetURL(base+`/attachments/1/${id}?copy_instance=${"f".repeat(32)}`),"");
});

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
  if (assets.length !== 20) {
    fail(at, `disk has ${assets.length} assets, want 20`);
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
  const staticChannel = diskChannel(inventory);
  const plotPath = "/api/nodes/drive/assets/a_plot";
  const channel = async (...args) => {
    if (args[0] === "/api/nodes/drive/chat") {
      return new Response(JSON.stringify({ assets: { a_plot: { url: plotPath } } }));
    }
    if (args[0] === plotPath) return new Response("plot", { headers: { "Content-Type": "image/png" } });
    return staticChannel(...args);
  };
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
  assert.equal(typeof appDeps.fetchImpl, "function");
  assert.match(appDeps.assetURL("/assets/agents/claude.svg"), /^blob:boot\//);
  assert.equal(appDeps.assetURL("/assets/missing.svg"), "");
  await (await appDeps.fetchImpl("/api/nodes/drive/chat")).json();
  assert.match(appDeps.assetURL(plotPath), /^blob:boot\//,
    "chat images must be resolved through the same channel as the application");
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

test("Unchanged capability poll preserves an in-flight People download", async()=>{
 const server="a".repeat(32),owner="b".repeat(32),chat="c".repeat(16),gen="d".repeat(16),id="0123456789abcdef";
 const root="/api/team/subscriptions",base=`${root}/${server}/${owner}/${chat}/${gen}`,file=`${base}/attachments/1/${id}?copy_instance=${"f".repeat(32)}`;
 const capabilities={people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:"supported"}],subscriptions:[{server,owner,chat,generation:gen,people_copy_instance:"f".repeat(32)}]};
 let release,startedResolve;const started=new Promise(resolve=>{startedResolve=resolve});
 const assets=new Map();
 const transport=createRemoteAssetTransport({assets,createObjectURL:()=>"blob:verified-synthetic",channel:async path=>{
  if(path===root)return new Response(JSON.stringify(capabilities),{headers:{"Content-Type":"application/json"}});
  if(path.split("?")[0]===base+"/comments")return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id,size:1,mime:"text/plain"}]}]}),{headers:{"Content-Type":"application/json"}});
  assert.equal(path,file);startedResolve();return await new Promise(resolve=>{release=()=>resolve(new Response("x",{headers:{"Content-Type":"application/octet-stream","Content-Length":"1"}}))});
 }});
 await (await transport.fetchImpl(root)).json();
 const comments=await transport.fetchImpl(base+"/comments?copy_instance="+"f".repeat(32));const pending=comments.json();await started;
 await (await transport.fetchImpl(root)).json();
 release();await pending;
 assert.equal(transport.assetURL(file),"blob:verified-synthetic","unchanged capabilities discarded an authorized in-flight download");
});

test("remote download waits for an overlapping capability refresh before exposing bytes", async()=>{
 const server="a".repeat(32),owner="b".repeat(32),chat="c".repeat(16),gen="d".repeat(16),instance="f".repeat(32),id="0123456789abcdef";
 const root="/api/team/subscriptions",base=`${root}/${server}/${owner}/${chat}/${gen}`,file=`${base}/attachments/1/${id}?copy_instance=${instance}`;
 const limits={version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216};
 for(const staysSupported of [true,false]) {
  let releaseFile,releasePoll,entered;const started=new Promise(resolve=>{entered=resolve});let polls=0;
  const caps=supported=>({people_attachments:limits,servers:[{id:server,people_files:supported?"supported":"unsupported"}],subscriptions:[{server,owner,chat,generation:gen,people_copy_instance:instance}]});
  const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=>"blob:verified-synthetic",channel:async path=>{
   if(path===root){if(++polls===1)return new Response(JSON.stringify(caps(true)));return new Promise(resolve=>{releasePoll=()=>resolve(new Response(JSON.stringify(caps(staysSupported))))});}
   if(path.split("?")[0]===base+"/comments")return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id,size:1,mime:"text/plain"}]}]}));
   assert.equal(path,file);entered();return new Promise(resolve=>{releaseFile=()=>resolve(new Response("x",{headers:{"Content-Type":"application/octet-stream"}}))});
  }});
  await (await transport.fetchImpl(root)).json();
  const pending=(await transport.fetchImpl(base+"/comments?copy_instance="+instance)).json();await started;
  const refresh=transport.fetchImpl(root);releaseFile();await new Promise(resolve=>setImmediate(resolve));
  assert.equal(transport.assetURL(file),"","bytes were exposed while authorization was unresolved");
  releasePoll();await (await refresh).json();await pending;
  assert.equal(transport.assetURL(file),staysSupported?"blob:verified-synthetic":"");
 }
});

test("remote capability failures revoke cached People files and block retries", async()=>{
 const server="a".repeat(32),owner="b".repeat(32),chat="c".repeat(16),generation="d".repeat(16),instance="f".repeat(32),root="/api/team/subscriptions",copy=`${root}/${server}/${owner}/${chat}/${generation}`;
 const file=`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`;
 const caps={people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:"supported"}],subscriptions:[{server,owner,chat,generation,people_copy_instance:instance}]};
 for(const failure of ["network","invalid JSON"]) {
  let polls=0,downloads=0;const revoked=[];
  const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=>"blob:cached",revokeObjectURL:url=>revoked.push(url),channel:async path=>{
   if(path===root){if(++polls===1)return new Response(JSON.stringify(caps));if(failure==="network")throw Error("synthetic offline");return new Response("{");}
   assert.equal(path,file);downloads++;return new Response("x",{headers:{"Content-Type":"application/octet-stream"}});
  }});
  await (await transport.fetchImpl(root)).json();assert.equal(await transport.retryAsset(file),"blob:cached");
  if(failure==="network")await assert.rejects(transport.fetchImpl(root),/synthetic offline/);
  else await assert.rejects((await transport.fetchImpl(root)).json());
  assert.equal(transport.assetURL(file),"");assert.deepEqual(revoked,["blob:cached"]);
  assert.equal(await transport.retryAsset(file),"");assert.equal(downloads,1);
 }
});

test("a real capability change retires an in-flight People download even after support returns", async()=>{
 const server="a".repeat(32),owner="b".repeat(32),chat="c".repeat(16),gen="d".repeat(16),id="0123456789abcdef";
 const root="/api/team/subscriptions",base=`${root}/${server}/${owner}/${chat}/${gen}`,file=`${base}/attachments/1/${id}?copy_instance=${"f".repeat(32)}`;
 const limits={version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216};let support=true,release,startedResolve;const started=new Promise(r=>{startedResolve=r});
 const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=>"blob:synthetic",channel:async path=>{
  if(path===root)return new Response(JSON.stringify({people_attachments:limits,servers:[{id:server,people_files:support?"supported":"unsupported"}],subscriptions:[{server,owner,chat,generation:gen,people_copy_instance:"f".repeat(32)}]}));
  if(path.split("?")[0]===base+"/comments")return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id,size:1,mime:"text/plain"}]}]}));
  startedResolve();return new Promise(r=>{release=()=>r(new Response("x",{headers:{"Content-Type":"application/octet-stream"}}))});
 }});
 await (await transport.fetchImpl(root)).json();const pending=(await transport.fetchImpl(base+"/comments?copy_instance="+"f".repeat(32))).json();await started;
 support=false;await (await transport.fetchImpl(root)).json();support=true;await (await transport.fetchImpl(root)).json();release();await pending;
 assert.equal(transport.assetURL(file),"");
});

test("remote copy replacement retires a held file and refuses the old identity", async()=>{
 const server="a".repeat(32),owner="b".repeat(32),chat="c".repeat(16),generation="d".repeat(16),root="/api/team/subscriptions",base=`${root}/${server}/${owner}/${chat}/${generation}`;
 const old="e".repeat(32),replacement="f".repeat(32),file=`${base}/attachments/1/0123456789abcdef?copy_instance=${old}`;let current=old,release,entered;const started=new Promise(resolve=>{entered=resolve});
 const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=>"blob:synthetic",channel:async path=>{
  if(path===root)return new Response(JSON.stringify({people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:"supported"}],subscriptions:[{server,owner,chat,generation,people_copy_instance:current}]}));
  if(path.split("?")[0]===base+"/comments")return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id:"0123456789abcdef",size:1,mime:"text/plain"}]}]}));
  assert.equal(path,file);entered();return new Promise(resolve=>{release=()=>resolve(new Response("x",{headers:{"Content-Type":"application/octet-stream"}}))});
 }});
 await (await transport.fetchImpl(root)).json();const reading=(await transport.fetchImpl(base+"/comments?copy_instance="+old)).json();await started;
 current=replacement;await (await transport.fetchImpl(root)).json();release();await reading;
 assert.equal(transport.assetURL(file),"");assert.equal(await transport.retryAsset(file),"");
});

test("remote stale history cannot prefetch using a replacement copy identity", async()=>{
 const server="a".repeat(32),owner="b".repeat(32),chat="c".repeat(16),generation="d".repeat(16),root="/api/team/subscriptions",base=`${root}/${server}/${owner}/${chat}/${generation}`;
 const old="e".repeat(32),replacement="f".repeat(32);let fileRequests=0;
 const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=>"blob:synthetic",channel:async path=>{
  if(path===root)return new Response(JSON.stringify({people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:"supported"}],subscriptions:[{server,owner,chat,generation,people_copy_instance:replacement}]}));
  if(path.startsWith(base+"/comments"))return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id:"0123456789abcdef",size:1,mime:"text/plain"}]}]}));
  fileRequests++;return new Response("x",{headers:{"Content-Type":"application/octet-stream"}});
 }});
 await (await transport.fetchImpl(root)).json();await (await transport.fetchImpl(base+"/comments?after=0&copy_instance="+old)).json();
 assert.equal(fileRequests,0);
});

test("remote multi-file prefetch stops when its copy or server binding is retired", async()=>{
 for(const reason of ['copy replacement','server change','copy removal']) {
  const server='a'.repeat(32),owner='b'.repeat(32),chat='c'.repeat(16),generation='d'.repeat(16),root='/api/team/subscriptions',base=`${root}/${server}/${owner}/${chat}/${generation}`;
  const old='e'.repeat(32),replacement='f'.repeat(32);let current=old,binding='first',release,entered;const started=new Promise(resolve=>{entered=resolve}),fileRequests=[];
  const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=> 'blob:synthetic',channel:async(path,opts)=>{
   if(path===root)return new Response(JSON.stringify({people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:'supported',people_file_binding:binding}],subscriptions:[{server,owner,chat,generation,people_copy_instance:current}]}));
   if(path===base&&opts?.method==='DELETE')return new Response(null,{status:204});
   if(path.startsWith(base+'/comments'))return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id:'0123456789abcdef',size:1,mime:'image/png'},{id:'fedcba9876543210',size:1,mime:'text/plain'}]},{seq:2,attachments:[{id:'1111111111111111',size:1,mime:'text/plain'}]}]}));
   fileRequests.push(path);if(fileRequests.length===1){entered();return new Promise(resolve=>{release=()=>resolve(new Response('x',{headers:{'Content-Type':'application/octet-stream'}}))});}
   return new Response('x',{headers:{'Content-Type':'application/octet-stream'}});
  }});
  await (await transport.fetchImpl(root)).json();const reading=(await transport.fetchImpl(base+'/comments?copy_instance='+old)).json();await started;
  if(reason==='copy removal')await transport.fetchImpl(base,{method:'DELETE'});
  else {if(reason==='copy replacement')current=replacement;else binding='second';await (await transport.fetchImpl(root)).json();}
  release();await reading;
  assert.deepEqual(fileRequests,[base+'/attachments/1/0123456789abcdef?copy_instance='+old],reason+' must retire remaining original files and preview');
 }
});

test("unchanged copy polling preserves multi-file prefetch and raster preview", async()=>{
 const server='a'.repeat(32),owner='b'.repeat(32),chat='c'.repeat(16),generation='d'.repeat(16),copyInstance='e'.repeat(32),root='/api/team/subscriptions',base=`${root}/${server}/${owner}/${chat}/${generation}`;
 let release,entered;const started=new Promise(resolve=>{entered=resolve}),files=[];
 const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=> 'blob:synthetic',channel:async path=>{
  if(path===root)return new Response(JSON.stringify({people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:'supported',people_file_binding:'same'}],subscriptions:[{server,owner,chat,generation,people_copy_instance:copyInstance}]}));
  if(path.startsWith(base+'/comments'))return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id:'0123456789abcdef',size:1,mime:'image/png'},{id:'fedcba9876543210',size:1,mime:'text/plain'}]},{seq:2,attachments:[{id:'1111111111111111',size:1,mime:'text/plain'}]}]}));
  files.push(path);if(files.length===1){entered();return new Promise(resolve=>{release=()=>resolve(new Response('x',{headers:{'Content-Type':'application/octet-stream'}}))});}
  return new Response('x',{headers:{'Content-Type':path.endsWith('&preview=1')?'image/png':'application/octet-stream'}});
 }});
 await (await transport.fetchImpl(root)).json();const reading=(await transport.fetchImpl(base+'/comments?copy_instance='+copyInstance)).json();await started;
 await (await transport.fetchImpl(root)).json();release();await reading;
 assert.deepEqual(files,[base+'/attachments/1/0123456789abcdef?copy_instance='+copyInstance,base+'/attachments/1/0123456789abcdef?copy_instance='+copyInstance+'&preview=1',base+'/attachments/1/fedcba9876543210?copy_instance='+copyInstance,base+'/attachments/2/1111111111111111?copy_instance='+copyInstance]);
});

test('remote associated-file retry requires verified capability and returns only a fresh scoped asset',async()=>{
 const server='a'.repeat(32),key=[server,'b'.repeat(32),'c'.repeat(16),'d'.repeat(16)].join('/'),root='/api/team/subscriptions',file=`${root}/${key}/attachments/1/0123456789abcdef?copy_instance=${"f".repeat(32)}`;
 const caps={people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:'supported'}],subscriptions:[{server,owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),people_copy_instance:'f'.repeat(32)}]};let gets=0;
 const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=> 'blob:synthetic-retry',channel:async path=>{if(path===root)return new Response(JSON.stringify(caps));gets++;return gets===1?new Response('offline',{status:503}):new Response('x',{headers:{'Content-Type':'application/octet-stream'}})}});
 assert.equal(await transport.retryAsset(file),'');assert.equal(gets,0);
 await (await transport.fetchImpl(root)).json();assert.equal(await transport.retryAsset(file),'');assert.equal(gets,1);
 assert.equal(await transport.retryAsset(file),'blob:synthetic-retry');assert.equal(gets,2);
 assert.equal(await transport.retryAsset('/api/nodes/n/send'),'');assert.equal(gets,2);
});

test('remote copy removal revokes an actual object URL with the default adapter',async()=>{
 const server='a'.repeat(32),owner='b'.repeat(32),chat='c'.repeat(16),generation='d'.repeat(16),instance='f'.repeat(32),root='/api/team/subscriptions',copy=`${root}/${server}/${owner}/${chat}/${generation}`,file=`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`;
 const caps={people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:'supported'}],subscriptions:[{server,owner,chat,generation,people_copy_instance:instance}]};
 const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:blob=>URL.createObjectURL(blob),channel:async(path,opts)=>path===root?new Response(JSON.stringify(caps)):opts?.method==='DELETE'?new Response(null,{status:204}):new Response('x',{headers:{'Content-Type':'application/octet-stream'}})});
 await (await transport.fetchImpl(root)).json();const url=await transport.retryAsset(file);assert.equal(await (await fetch(url)).text(),'x');
 await transport.fetchImpl(copy,{method:'DELETE'});assert.equal(transport.assetURL(file),'');await assert.rejects(fetch(url));
});

test('remote associated files reject refused, empty, oversized and unsafe responses',async()=>{
 const server='a'.repeat(32),owner='b'.repeat(32),chat='c'.repeat(16),generation='d'.repeat(16),instance='f'.repeat(32),root='/api/team/subscriptions',copy=`${root}/${server}/${owner}/${chat}/${generation}`,file=`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`;
 const caps={people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:'supported'}],subscriptions:[{server,owner,chat,generation,people_copy_instance:instance}]};
 const responses=[()=>new Response('denied',{status:403}),()=>new Response('x',{headers:{'Content-Length':'4194305','Content-Type':'application/octet-stream'}}),()=>new Response('',{headers:{'Content-Type':'application/octet-stream'}}),()=>new Response(new Uint8Array(4194305),{headers:{'Content-Type':'application/octet-stream'}}),()=>new Response('x',{headers:{'Content-Type':'text/html'}})];
 for(const make of responses){let created=0;const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=>{created++;return 'blob:unexpected'},channel:async path=>path===root?new Response(JSON.stringify(caps)):make()});
  await (await transport.fetchImpl(root)).json();assert.equal(await transport.retryAsset(file),'');assert.equal(created,0);
 }
});

test('remote comments without a copy precondition cannot prefetch associated files',async()=>{
 const server='a'.repeat(32),owner='b'.repeat(32),chat='c'.repeat(16),generation='d'.repeat(16),instance='f'.repeat(32),root='/api/team/subscriptions',copy=`${root}/${server}/${owner}/${chat}/${generation}`;let reads=0;
 const caps={people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:'supported'}],subscriptions:[{server,owner,chat,generation,people_copy_instance:instance}]};
 const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=>assert.fail('unbound file was loaded'),channel:async path=>{
  if(path===root)return new Response(JSON.stringify(caps));
  if(path===copy+'/comments')return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id:'0123456789abcdef',size:1,mime:'text/plain'}]}]}));
  reads++;return new Response('x',{headers:{'Content-Type':'application/octet-stream'}});
 }});
 await (await transport.fetchImpl(root)).json();const history=await (await transport.fetchImpl(copy+'/comments')).json();assert.equal(history.entries.length,1);assert.equal(reads,0);
});

test('remote People preview refuses unsafe image types while retaining the download',async()=>{
 const server='a'.repeat(32),owner='b'.repeat(32),chat='c'.repeat(16),generation='d'.repeat(16),instance='f'.repeat(32),root='/api/team/subscriptions',copy=`${root}/${server}/${owner}/${chat}/${generation}`;
 const file=`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`,preview=file+'&preview=1';
 const caps={people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:'supported'}],subscriptions:[{server,owner,chat,generation,people_copy_instance:instance}]};
 for(const type of ['image/gif','text/html']) {
  const fetched=[];const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=> 'blob:download',channel:async path=>{
   if(path===root)return new Response(JSON.stringify(caps));
   if(path.startsWith(copy+'/comments'))return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id:'0123456789abcdef',size:1,mime:'image/png'}]}]}));
   fetched.push(path);return new Response('x',{headers:{'Content-Type':path===preview?type:'application/octet-stream'}});
  }});
  await (await transport.fetchImpl(root)).json();await (await transport.fetchImpl(copy+'/comments?copy_instance='+instance)).json();
  assert.deepEqual(fetched,[file,preview]);assert.equal(transport.assetURL(file),'blob:download');assert.equal(transport.assetURL(preview),'');
 }
});

test('remote native attachment preload preserves its existing size and type boundary',async()=>{
 const route='/api/nodes/n1/attachments',file=route+'/01234567-synthetic.txt';
 for(const [headers,expected] of [[{'Content-Type':'application/octet-stream'},'blob:valid'],[{'Content-Type':'text/html'},''],[{'Content-Type':'application/octet-stream','Content-Length':String(25*1024*1024+1)},'']]){
  const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=> 'blob:valid',channel:async(path,opts)=>opts?.method==='POST'?new Response(JSON.stringify({attachments:[{path:'/synthetic/01234567-synthetic.txt'}]})):new Response('x',{headers})});
  await (await transport.fetchImpl(route+'?chat_instance=synthetic',{method:'POST'})).json();assert.equal(transport.assetURL(file),expected);
 }
 let created=0;
 const oversized=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=>{created++;return 'blob:unexpected'},channel:async(path,opts)=>opts?.method==='POST'?new Response(JSON.stringify({attachments:[{path:'/synthetic/01234567-synthetic.txt'}]})):new Response(new Uint8Array(25*1024*1024+1),{headers:{'Content-Type':'application/octet-stream'}})});
 await (await oversized.fetchImpl(route+'?chat_instance=synthetic',{method:'POST'})).json();
 assert.equal(oversized.assetURL(file),'');assert.equal(created,0);
});

test('remote native preloads evict the oldest file to stay within the shared byte budget',async()=>{
	const route='/api/nodes/n1/attachments',assets=new Map();let created=0;
	const transport=createRemoteAssetTransport({assets,peopleByteBudget:1,createObjectURL:()=>`blob:synthetic-${++created}`,revokeObjectURL:()=>{},channel:async(path,opts)=>opts?.method==='POST'?new Response(JSON.stringify({attachments:[{path:'/synthetic/01234567-first.txt'},{path:'/synthetic/76543210-second.txt'}]})):new Response('x',{headers:{'Content-Type':'application/octet-stream'}})});
 await (await transport.fetchImpl(route,{method:'POST'})).json();
	assert.equal(transport.assetURL(route+'/01234567-first.txt'),'');assert.equal(transport.assetURL(route+'/76543210-second.txt'),'blob:synthetic-2');assert.equal(created,2);assert.equal(assets.size,1);
});

test('remote native preload evicts an older file when the new one fits the byte budget',async()=>{
 const route='/api/nodes/n1/attachments';let created=0,reads=0;
 const transport=createRemoteAssetTransport({assets:new Map(),peopleByteBudget:2,createObjectURL:()=>`blob:synthetic-${++created}`,revokeObjectURL:()=>{},channel:async(path,opts)=>opts?.method==='POST'?new Response(JSON.stringify({attachments:[{path:'/synthetic/01234567-first.txt'},{path:'/synthetic/76543210-second.txt'}]})):(reads++,new Response(path.includes('first')?'x':'xx',{headers:{'Content-Type':'application/octet-stream'}}))});
 await (await transport.fetchImpl(route,{method:'POST'})).json();assert.equal(transport.assetURL(route+'/01234567-first.txt'),'');assert.equal(transport.assetURL(route+'/76543210-second.txt'),'blob:synthetic-2');assert.equal(created,2);assert.equal(reads,2);
});

test('remote native upload preload ignores malformed attachment addresses',async()=>{
 const route='/api/nodes/n1/attachments',requested=[];
 const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=> 'blob:valid',channel:async(path,opts)=>{
  if(opts?.method==='POST')return new Response(JSON.stringify({attachments:[null,{}, {path:1},{path:'/synthetic/not-an-id.txt'},{path:'/synthetic/01234567-bad\\name.txt'},{path:'/synthetic/01234567-good.txt'}]}));
  requested.push(path);return new Response('x',{headers:{'Content-Type':'application/octet-stream'}});
 }});
 await (await transport.fetchImpl(route+'?chat_instance=synthetic',{method:'POST'})).json();
 assert.deepEqual(requested,[route+'/01234567-good.txt']);
});

test('remote upload prefetch rejects an overfull response without fetching any file',async()=>{
 const route='/api/nodes/n1/attachments';let reads=0;
 const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=>assert.fail('overfull response created an asset'),channel:async(path,opts)=>{
  if(opts?.method==='POST')return new Response(JSON.stringify({attachments:Array.from({length:17},(_,i)=>({path:`/synthetic/${String(i).padStart(8,'0')}-file.txt`}))}));
  reads++;return new Response('x',{headers:{'Content-Type':'application/octet-stream'}});
 }});
 const response=await transport.fetchImpl(route,{method:'POST'});assert.equal((await response.json()).attachments.length,17);assert.equal(reads,0);
});

test('remote prefetch ignores malformed file associations and limits one response',async()=>{
 const server='a'.repeat(32),owner='b'.repeat(32),chat='c'.repeat(16),generation='d'.repeat(16),instance='f'.repeat(32),root='/api/team/subscriptions',copy=`${root}/${server}/${owner}/${chat}/${generation}`;
 const caps={people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:'supported'}],subscriptions:[{server,owner,chat,generation,people_copy_instance:instance}]};let files=0;
 const malformed=[{seq:0,attachments:[{id:'0123456789abcdef',size:1}]},{seq:1,attachments:[{id:'bad',size:1},{id:'0123456789abcdef',size:0},{id:'0123456789abcdef',size:4194305}]},{seq:2,attachments:Array.from({length:17},()=>({id:'0123456789abcdef',size:1}))},...Array.from({length:17},(_,i)=>({seq:i+3,attachments:Array.from({length:16},()=>({id:'bad',size:1}))}))];
 const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=>assert.fail('invalid file loaded'),channel:async path=>{if(path===root)return new Response(JSON.stringify(caps));if(path.startsWith(copy+'/comments'))return new Response(JSON.stringify({entries:malformed}));files++;return new Response('x');}});
 await (await transport.fetchImpl(root)).json();await (await transport.fetchImpl(copy+'/comments?copy_instance='+instance)).json();assert.equal(files,0);
});

test('remote explicit retry cannot revive a deleted copy or bypass a capability refresh',async()=>{
 const server='a'.repeat(32),key=[server,'b'.repeat(32),'c'.repeat(16),'d'.repeat(16)].join('/'),root='/api/team/subscriptions',copy=`${root}/${key}`,file=`${copy}/attachments/1/0123456789abcdef?copy_instance=${"f".repeat(32)}`;
 const caps={people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:'supported'}],subscriptions:[{server,owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),people_copy_instance:'f'.repeat(32)}]};let release,gets=0;
 const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=> 'blob:must-not-revive',channel:async(path,opts)=>{if(path===root)return new Response(JSON.stringify(caps));if(path===copy&&opts?.method==='DELETE')return new Response(null,{status:204});gets++;return new Promise(resolve=>{release=()=>resolve(new Response('x',{headers:{'Content-Type':'application/octet-stream'}}))})}});
 await (await transport.fetchImpl(root)).json();const refreshing=await transport.fetchImpl(root);assert.equal(await transport.retryAsset(file),'');assert.equal(gets,0);await refreshing.json();
 const pending=transport.retryAsset(file);await new Promise(resolve=>setImmediate(resolve));assert.equal(gets,1);await transport.fetchImpl(copy,{method:'DELETE'});release();assert.equal(await pending,'');assert.equal(transport.assetURL(file),'');
});

test('remote query-bound copy removal retires an in-flight associated file',async()=>{
 const server='a'.repeat(32),owner='b'.repeat(32),chat='c'.repeat(16),generation='d'.repeat(16),instance='f'.repeat(32),root='/api/team/subscriptions',copy=`${root}/${server}/${owner}/${chat}/${generation}`,file=`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`;
 const caps={people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:'supported'}],subscriptions:[{server,owner,chat,generation,people_copy_instance:instance}]};let release,started;const entered=new Promise(resolve=>{started=resolve});
 const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=> 'blob:must-not-revive',channel:async(path,opts)=>{if(path===root)return new Response(JSON.stringify(caps));if(opts?.method==='DELETE')return new Response(null,{status:204});started();return new Promise(resolve=>{release=()=>resolve(new Response('x',{headers:{'Content-Type':'application/octet-stream'}}))})}});
 await (await transport.fetchImpl(root)).json();const pending=transport.retryAsset(file);await entered;
 await transport.fetchImpl(copy+'?copy_instance='+instance,{method:'DELETE'});release();assert.equal(await pending,'');assert.equal(transport.assetURL(file),'');
});

test('late remote removal completion cannot discard a replacement copy asset',async()=>{
 const server='a'.repeat(32),owner='b'.repeat(32),chat='c'.repeat(16),generation='d'.repeat(16),old='e'.repeat(32),fresh='f'.repeat(32),root='/api/team/subscriptions',copy=`${root}/${server}/${owner}/${chat}/${generation}`,file=`${copy}/attachments/1/0123456789abcdef?copy_instance=${fresh}`;
 let current=old,release,started;const entered=new Promise(resolve=>{started=resolve});const revoked=[];
 const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=> 'blob:replacement',revokeObjectURL:url=>revoked.push(url),channel:async(path,opts)=>{
  if(path===root)return new Response(JSON.stringify({people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:'supported'}],subscriptions:[{server,owner,chat,generation,people_copy_instance:current}]}));
  if(opts?.method==='DELETE'){started();return new Promise(resolve=>{release=()=>resolve(new Response(null,{status:204}))});}
  return new Response('x',{headers:{'Content-Type':'application/octet-stream'}});
 }});
 await (await transport.fetchImpl(root)).json();const removing=transport.fetchImpl(copy+'?copy_instance='+old,{method:'DELETE'});await entered;
 current=fresh;await (await transport.fetchImpl(root)).json();assert.equal(await transport.retryAsset(file),'blob:replacement');release();await removing;
 assert.equal(transport.assetURL(file),'blob:replacement');assert.deepEqual(revoked,[]);
});

test('remote file prefetch rejects malformed copy capability metadata and resumes when repaired',async()=>{
 const server='a'.repeat(32),owner='b'.repeat(32),chat='c'.repeat(16),generation='d'.repeat(16),instance='f'.repeat(32),root='/api/team/subscriptions',copy=`${root}/${server}/${owner}/${chat}/${generation}`,file=`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`;
 const limits={version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216}, valid={server,owner,chat,generation,people_copy_instance:instance};
 for (const field of ['server','owner','chat','generation','people_copy_instance']) for(const value of ['invalid',undefined]) {
  let row={...valid,[field]:value}, downloads=0;
  const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=> 'blob:verified',channel:async path=>{
   if(path===root)return new Response(JSON.stringify({people_attachments:limits,servers:[{id:server,people_files:'supported'}],subscriptions:[row]}));
   if(path.startsWith(copy+'/comments'))return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id:'0123456789abcdef',size:1,mime:'text/plain'}]}]}));
   downloads++;return new Response('x',{headers:{'Content-Type':'application/octet-stream'}});
  }});
  await (await transport.fetchImpl(root)).json();await (await transport.fetchImpl(copy+'/comments?copy_instance='+instance)).json();
  assert.equal(downloads,0,field+' cannot authorize a download');assert.equal(transport.assetURL(file),'');
  row=valid;await (await transport.fetchImpl(root)).json();await (await transport.fetchImpl(copy+'/comments?copy_instance='+instance)).json();
  assert.equal(downloads,1,field+' recovery must permit the verified association');assert.equal(transport.assetURL(file),'blob:verified');
 }
});

test('remote People prefetch waits for a refresh begun while exposing the first file',async()=>{
 const server='a'.repeat(32),owner='b'.repeat(32),chat='c'.repeat(16),generation='d'.repeat(16),instance='f'.repeat(32),root='/api/team/subscriptions',copy=`${root}/${server}/${owner}/${chat}/${generation}`;
 const caps={people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:'supported'}],subscriptions:[{server,owner,chat,generation,people_copy_instance:instance}]};
 const files=[],first=`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`,second=`${copy}/attachments/1/fedcba9876543210?copy_instance=${instance}`;let polls=0,release,started,refresh,transport;
 const entered=new Promise(resolve=>{started=resolve});
 transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=>{if(!refresh)refresh=transport.fetchImpl(root);return 'blob:verified'},channel:async path=>{
  if(path===root){polls++;if(polls===1)return new Response(JSON.stringify(caps));started();return new Promise(resolve=>{release=()=>resolve(new Response(JSON.stringify(caps)))})}
  if(path.startsWith(copy+'/comments'))return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id:'0123456789abcdef',size:1,mime:'text/plain'},{id:'fedcba9876543210',size:1,mime:'text/plain'}]}]}));
  files.push(path);return new Response('x',{headers:{'Content-Type':'application/octet-stream'}});
 }});
 await (await transport.fetchImpl(root)).json();const reading=(await transport.fetchImpl(copy+'/comments?copy_instance='+instance)).json();await entered;
 assert.deepEqual(files,[first]);release();await (await refresh).json();await reading;
 assert.deepEqual(files,[first,second]);assert.equal(transport.assetURL(second),'blob:verified');
});

test('remote People prefetch stops when an overlapping capability refresh times out',async t=>{
 t.mock.timers.enable({apis:['setTimeout']});
 const server='a'.repeat(32),owner='b'.repeat(32),chat='c'.repeat(16),generation='d'.repeat(16),instance='f'.repeat(32),root='/api/team/subscriptions',copy=`${root}/${server}/${owner}/${chat}/${generation}`;
 const caps={people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:'supported'}],subscriptions:[{server,owner,chat,generation,people_copy_instance:instance}]};
 const files=[],first=`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`,second=`${copy}/attachments/1/fedcba9876543210?copy_instance=${instance}`;let polls=0,release,started,refresh,transport;
 const entered=new Promise(resolve=>{started=resolve});
 transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=>{if(!refresh)refresh=transport.fetchImpl(root);return 'blob:verified'},channel:async path=>{
  if(path===root){polls++;if(polls===1)return new Response(JSON.stringify(caps));started();return new Promise(resolve=>{release=()=>resolve(new Response(JSON.stringify(caps)))})}
  if(path.startsWith(copy+'/comments'))return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id:'0123456789abcdef',size:1,mime:'text/plain'},{id:'fedcba9876543210',size:1,mime:'text/plain'}]}]}));
  files.push(path);return new Response('x',{headers:{'Content-Type':'application/octet-stream'}});
 }});
 await (await transport.fetchImpl(root)).json();const reading=(await transport.fetchImpl(copy+'/comments?copy_instance='+instance)).json();await entered;
 try {
  await new Promise(resolve=>setImmediate(resolve));t.mock.timers.tick(10000);await reading;
  assert.deepEqual(files,[first]);assert.equal(transport.assetURL(second),'');
 } finally {release();await (await refresh).json()}
});

test('remote People prefetch stops when an overlapping refresh replaces the copy',async()=>{
 const server='a'.repeat(32),owner='b'.repeat(32),chat='c'.repeat(16),generation='d'.repeat(16),instance='f'.repeat(32),root='/api/team/subscriptions',copy=`${root}/${server}/${owner}/${chat}/${generation}`;
 const caps={people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:'supported'}],subscriptions:[{server,owner,chat,generation,people_copy_instance:instance}]};
 const replacement={...caps,subscriptions:[{server,owner,chat,generation,people_copy_instance:'e'.repeat(32)}]};
 const files=[],first=`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`,second=`${copy}/attachments/1/fedcba9876543210?copy_instance=${instance}`;let polls=0,release,started,refresh,transport;
 const entered=new Promise(resolve=>{started=resolve});
 transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=>{if(!refresh)refresh=transport.fetchImpl(root);return 'blob:old'},channel:async path=>{
  if(path===root){polls++;if(polls===1)return new Response(JSON.stringify(caps));started();return new Promise(resolve=>{release=()=>resolve(new Response(JSON.stringify(replacement)))})}
  if(path.startsWith(copy+'/comments'))return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id:'0123456789abcdef',size:1,mime:'text/plain'},{id:'fedcba9876543210',size:1,mime:'text/plain'}]}]}));
  files.push(path);return new Response('x',{headers:{'Content-Type':'application/octet-stream'}});
 }});
 await (await transport.fetchImpl(root)).json();const reading=(await transport.fetchImpl(copy+'/comments?copy_instance='+instance)).json();await entered;
 assert.deepEqual(files,[first]);release();await (await refresh).json();await reading;
 assert.deepEqual(files,[first]);assert.equal(transport.assetURL(first),'');assert.equal(transport.assetURL(second),'');
});

test('a server binding change during image preview stops the next file prefetch',async()=>{
 const server='a'.repeat(32),owner='b'.repeat(32),chat='c'.repeat(16),generation='d'.repeat(16),instance='e'.repeat(32),root='/api/team/subscriptions',copy=`${root}/${server}/${owner}/${chat}/${generation}`;
 const caps={people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:'supported',people_file_binding:'a'.repeat(64)}],subscriptions:[{server,owner,chat,generation,people_copy_instance:instance}]};
 const changed={...caps,servers:[{...caps.servers[0],people_file_binding:'b'.repeat(64)}]};
 const first=`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`,preview=first+'&preview=1',second=`${copy}/attachments/1/fedcba9876543210?copy_instance=${instance}`;
 const png=Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAEUlEQVR4nGP4z8DwH4QZYAwAR8oH+WdZbrcAAAAASUVORK5CYII=','base64');
 let polls=0,release,entered,dispatches=0;const waiting=new Promise(resolve=>{entered=resolve}),requests=[];
 const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=> 'blob:synthetic',revokeObjectURL:()=>{},channel:async(path,options)=>{
  if(options?.method && options.method!=='GET'){dispatches++;throw Error('unexpected synthetic mutation')}
  if(path===root)return new Response(JSON.stringify(++polls===1?caps:changed));
  if(path.startsWith(copy+'/comments'))return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id:'0123456789abcdef',size:png.length,mime:'image/png'},{id:'fedcba9876543210',size:1,mime:'text/plain'}]}]}));
  requests.push(path);
  if(path===preview){entered();return new Promise(resolve=>{release=()=>resolve(new Response(png,{headers:{'Content-Type':'image/png'}}))})}
  if(path===first)return new Response(png,{headers:{'Content-Type':'application/octet-stream'}});
  if(path===second)return new Response('x',{headers:{'Content-Type':'application/octet-stream'}});
  throw Error('unexpected synthetic path');
 }});
 await (await transport.fetchImpl(root)).json();const reading=(await transport.fetchImpl(copy+'/comments?copy_instance='+instance)).json();
 await waiting;await (await transport.fetchImpl(root)).json();release();await reading;
 assert.deepEqual(requests,[first,preview]);assert.equal(transport.assetURL(preview),'');assert.equal(transport.assetURL(second),'');assert.equal(dispatches,0);
});

test('remote People prefetch does not start a file after an earlier capability refresh replaces the copy',async()=>{
 const server='a'.repeat(32),owner='b'.repeat(32),chat='c'.repeat(16),generation='d'.repeat(16),instance='f'.repeat(32),root='/api/team/subscriptions',copy=`${root}/${server}/${owner}/${chat}/${generation}`;
 const caps={people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:'supported'}],subscriptions:[{server,owner,chat,generation,people_copy_instance:instance}]};
 let polls=0,release,downloads=0;
 const transport=createRemoteAssetTransport({assets:new Map(),channel:async path=>{
  if(path===root){if(++polls===1)return new Response(JSON.stringify(caps));return new Promise(resolve=>{release=()=>resolve(new Response(JSON.stringify({...caps,subscriptions:[{server,owner,chat,generation,people_copy_instance:'e'.repeat(32)}]})))})}
  if(path.startsWith(copy+'/comments'))return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id:'0123456789abcdef',size:1,mime:'text/plain'}]}]}));
  downloads++;return new Response('x',{headers:{'Content-Type':'application/octet-stream'}});
 }});
 await (await transport.fetchImpl(root)).json();const comments=await transport.fetchImpl(copy+'/comments?copy_instance='+instance);
 const refresh=transport.fetchImpl(root);const reading=comments.json();await new Promise(resolve=>setImmediate(resolve));release();await (await refresh).json();await reading;
 assert.equal(downloads,0);assert.equal(transport.assetURL(`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`),'');
});

test('remote in-flight file is not exposed when capability verification times out',async t=>{
 t.mock.timers.enable({apis:['setTimeout']});
 const server='a'.repeat(32),owner='b'.repeat(32),chat='c'.repeat(16),generation='d'.repeat(16),instance='f'.repeat(32),root='/api/team/subscriptions',copy=`${root}/${server}/${owner}/${chat}/${generation}`,file=`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`;
 const caps={people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:'supported'}],subscriptions:[{server,owner,chat,generation,people_copy_instance:instance}]};
 let polls=0,releaseFile,releaseCaps,fileStarted;const entered=new Promise(resolve=>{fileStarted=resolve});
 const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=> 'blob:unexpected',channel:async path=>{
  if(path===root){polls++;if(polls===1)return new Response(JSON.stringify(caps));return new Promise(resolve=>{releaseCaps=()=>resolve(new Response(JSON.stringify(caps)))})}
  if(path.startsWith(copy+'/comments'))return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id:'0123456789abcdef',size:1,mime:'text/plain'}]}]}));
  fileStarted();return new Promise(resolve=>{releaseFile=()=>resolve(new Response('x',{headers:{'Content-Type':'application/octet-stream'}}))});
 }});
 await (await transport.fetchImpl(root)).json();const reading=(await transport.fetchImpl(copy+'/comments?copy_instance='+instance)).json();await entered;
 const refresh=transport.fetchImpl(root);releaseFile();
 try {await new Promise(resolve=>setImmediate(resolve));t.mock.timers.tick(10000);await new Promise(resolve=>setImmediate(resolve));t.mock.timers.tick(10000);await reading;assert.equal(transport.assetURL(file),'')}
 finally {releaseCaps();await (await refresh).json()}
});

test('remote transport does not infer a People association from a non-string request',async()=>{
 let calls=0,created=0;
 const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=>{created++;return 'blob:unexpected'},channel:async()=>{calls++;return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id:'0123456789abcdef',size:1,mime:'text/plain'}]}]}))}});
 const request=new Request('http://synthetic.invalid/comments');const response=await transport.fetchImpl(request);await response.json();
 assert.equal(calls,1);assert.equal(created,0);assert.equal(transport.assetURL('/api/team/subscriptions/unknown'),'');
});

test('remote People prefetch requires a valid capability server identity',async()=>{
 const server='a'.repeat(32),owner='b'.repeat(32),chat='c'.repeat(16),generation='d'.repeat(16),instance='f'.repeat(32),root='/api/team/subscriptions',copy=`${root}/${server}/${owner}/${chat}/${generation}`,file=`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`;
 for(const invalid of [undefined,'invalid']) {
  let advertised=invalid,downloads=0;
  const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=> 'blob:verified',channel:async path=>{
   if(path===root)return new Response(JSON.stringify({people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:advertised,people_files:'supported'}],subscriptions:[{server,owner,chat,generation,people_copy_instance:instance}]}));
   if(path.startsWith(copy+'/comments'))return new Response(JSON.stringify({entries:[{seq:1,attachments:[{id:'0123456789abcdef',size:1,mime:'text/plain'}]}]}));
   downloads++;return new Response('x',{headers:{'Content-Type':'application/octet-stream'}});
  }});
  await (await transport.fetchImpl(root)).json();await (await transport.fetchImpl(copy+'/comments?copy_instance='+instance)).json();assert.equal(downloads,0);assert.equal(transport.assetURL(file),'');
  advertised=server;await (await transport.fetchImpl(root)).json();await (await transport.fetchImpl(copy+'/comments?copy_instance='+instance)).json();assert.equal(downloads,1);assert.equal(transport.assetURL(file),'blob:verified');
 }
});

function peopleCaps(server, owner, chat, generation, instance) {
  return {people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:server,people_files:'supported'}],subscriptions:[{server,owner,chat,generation,people_copy_instance:instance}]};
}

test('a failed capability poll releases a shared file wait without trusting the server', async () => {
  const server='a'.repeat(32), owner='b'.repeat(32), chat='c'.repeat(16), generation='d'.repeat(16), instance='f'.repeat(32);
  const root='/api/team/subscriptions', copy=`${root}/${server}/${owner}/${chat}/${generation}`;
  const file=`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`;
  const caps=peopleCaps(server, owner, chat, generation, instance);
  let polls=0, releaseFile, releasePoll, entered;
  const started=new Promise(resolve=>{entered=resolve});
  let files=0;
  const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=>'blob:should-not-appear',channel:async path=>{
    if(path===root){
      if(++polls===1)return new Response(JSON.stringify(caps));
      if(polls===2)return new Promise(resolve=>{releasePoll=()=>resolve(new Response(JSON.stringify({error:'unavailable'}),{status:500}))});
      return new Response(JSON.stringify(caps));
    }
    if(++files===1){entered();return new Promise(resolve=>{releaseFile=()=>resolve(new Response('x',{headers:{'Content-Type':'application/octet-stream'}}))})}
    return new Response('x',{headers:{'Content-Type':'application/octet-stream'}});
  }});
  await (await transport.fetchImpl(root)).json();
  const pending=transport.retryAsset(file);await started;
  const refresh=transport.fetchImpl(root);releaseFile();
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(transport.assetURL(file),'');
  releasePoll();
  const outcome=await Promise.race([
    pending.then(url=>({url})),
    new Promise(resolve=>setImmediate(()=>setImmediate(()=>resolve({stalled:true})))),
  ]);
  assert.equal(outcome.stalled,undefined,'a 5xx capability poll left the shared file wait pending');
  assert.equal(outcome.url,'');
  assert.equal(transport.assetURL(file),'');
  const body=await (await refresh).json();
  assert.equal(body.error,'unavailable');
  assert.equal(await transport.retryAsset(file),'');
  await (await transport.fetchImpl(root)).json();
  assert.equal(await transport.retryAsset(file),'blob:should-not-appear');
});

test('a stale capability failure does not clear a newer poll', async () => {
  const server='a'.repeat(32), owner='b'.repeat(32), chat='c'.repeat(16), generation='d'.repeat(16), instance='f'.repeat(32);
  const root='/api/team/subscriptions', copy=`${root}/${server}/${owner}/${chat}/${generation}`;
  const file=`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`;
  const caps=peopleCaps(server, owner, chat, generation, instance);
  let polls=0, releaseFile, releaseStale, releaseFresh, entered;
  const started=new Promise(resolve=>{entered=resolve});
  const transport=createRemoteAssetTransport({assets:new Map(),createObjectURL:()=>'blob:kept',channel:async path=>{
    if(path===root){
      if(++polls===1)return new Response(JSON.stringify(caps));
      if(polls===2)return new Promise(resolve=>{releaseStale=()=>resolve(new Response(JSON.stringify({error:'stale'}),{status:500}))});
      return new Promise(resolve=>{releaseFresh=()=>resolve(new Response(JSON.stringify(caps)))});
    }
    entered();return new Promise(resolve=>{releaseFile=()=>resolve(new Response('x',{headers:{'Content-Type':'application/octet-stream'}}))});
  }});
  await (await transport.fetchImpl(root)).json();
  const pending=transport.retryAsset(file);await started;
  const stale=transport.fetchImpl(root);
  const fresh=transport.fetchImpl(root);
  releaseFile();
  await new Promise(resolve=>setImmediate(resolve));
  releaseStale();
  await stale;
  const stillWaiting=await Promise.race([
    pending.then(()=>'released'),
    new Promise(resolve=>setImmediate(()=>setImmediate(()=>resolve('waiting')))),
  ]);
  assert.equal(stillWaiting,'waiting');
  releaseFresh();
  await (await fresh).json();
  assert.equal(await pending,'blob:kept');
  const staleBody=await (await stale).json();
  assert.equal(staleBody.error,'stale');
  assert.equal(transport.assetURL(file),'blob:kept');
});

test('people file cache evicts the oldest entry and allows that path again', async () => {
  const server='a'.repeat(32), owner='b'.repeat(32), chat='c'.repeat(16), generation='d'.repeat(16), instance='f'.repeat(32);
  const root='/api/team/subscriptions', copy=`${root}/${server}/${owner}/${chat}/${generation}`;
  const caps=peopleCaps(server, owner, chat, generation, instance);
  const ids=['0123456789abcdef','fedcba9876543210','0011223344556677'];
  const path=id=>`${copy}/attachments/1/${id}?copy_instance=${instance}`;
  const revoked=[], reads=[];
  let n=0;
  const assets=new Map();
  const transport=createRemoteAssetTransport({assets,peopleByteBudget:2,peopleAssetCap:2,createObjectURL:()=>`blob:synthetic-${++n}`,revokeObjectURL:url=>revoked.push(url),channel:async request=>{
    if(request===root)return new Response(JSON.stringify(caps));
    reads.push(request);return new Response('x',{headers:{'Content-Type':'application/octet-stream'}});
  }});
  await (await transport.fetchImpl(root)).json();
  assert.equal(await transport.retryAsset(path(ids[0])),'blob:synthetic-1');
  assert.equal(await transport.retryAsset(path(ids[1])),'blob:synthetic-2');
  assert.equal(await transport.retryAsset(path(ids[2])),'blob:synthetic-3');
  assert.equal(transport.assetURL(path(ids[0])),'');
  assert.equal(transport.assetURL(path(ids[1])),'blob:synthetic-2');
  assert.equal(transport.assetURL(path(ids[2])),'blob:synthetic-3');
  assert.deepEqual(revoked,['blob:synthetic-1']);
  assert.equal(assets.size,2);
  assert.equal(await transport.retryAsset(path(ids[0])),'blob:synthetic-4');
  assert.equal(reads.filter(item=>item===path(ids[0])).length,2);
  assert.equal(transport.assetURL(path(ids[0])),'blob:synthetic-4');
});

test('an in-flight people file is not evicted', async () => {
  const server='a'.repeat(32), owner='b'.repeat(32), chat='c'.repeat(16), generation='d'.repeat(16), instance='f'.repeat(32);
  const root='/api/team/subscriptions', copy=`${root}/${server}/${owner}/${chat}/${generation}`;
  const caps=peopleCaps(server, owner, chat, generation, instance);
  const flight=`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`;
  const older=`${copy}/attachments/1/fedcba9876543210?copy_instance=${instance}`;
  const newer=`${copy}/attachments/1/0011223344556677?copy_instance=${instance}`;
  const revoked=[];
  let release, n=0, entered;
  const started=new Promise(resolve=>{entered=resolve});
  const assets=new Map();
  const transport=createRemoteAssetTransport({assets,peopleByteBudget:1,createObjectURL:()=>`blob:synthetic-${++n}`,revokeObjectURL:url=>revoked.push(url),channel:async request=>{
    if(request===root)return new Response(JSON.stringify(caps));
    if(request===flight){entered();return new Promise(resolve=>{release=()=>resolve(new Response('x',{headers:{'Content-Type':'application/octet-stream'}}))})}
    return new Response('x',{headers:{'Content-Type':'application/octet-stream'}});
  }});
  await (await transport.fetchImpl(root)).json();
  const pending=transport.retryAsset(flight);await started;
  assert.equal(await transport.retryAsset(older),'blob:synthetic-1');
  assert.equal(await transport.retryAsset(newer),'blob:synthetic-2');
  assert.deepEqual(revoked,['blob:synthetic-1']);
  assert.equal(transport.assetURL(flight),'');
  release();
  assert.equal(await pending,'blob:synthetic-3');
  assert.equal(transport.assetURL(flight),'blob:synthetic-3');
  assert.deepEqual(revoked,['blob:synthetic-1','blob:synthetic-2']);
});

test('a people file larger than the byte budget is refused', async () => {
  const route='/api/nodes/n1/attachments';
  const revoked=[];
  let n=0;
  const assets=new Map();
  const transport=createRemoteAssetTransport({assets,peopleByteBudget:2,createObjectURL:()=>`blob:synthetic-${++n}`,revokeObjectURL:url=>revoked.push(url),channel:async(request,opts)=>{
    if(opts?.method==='POST')return new Response(JSON.stringify({attachments:[{path:'/synthetic/01234567-small.txt'},{path:'/synthetic/76543210-large.txt'}]}));
    return new Response(request.endsWith('large.txt')?'xyz':'x',{headers:{'Content-Type':'application/octet-stream'}});
  }});
  await (await transport.fetchImpl(route,{method:'POST'})).json();
  assert.equal(transport.assetURL(route+'/01234567-small.txt'),'blob:synthetic-1');
  assert.equal(transport.assetURL(route+'/76543210-large.txt'),'');
  assert.deepEqual(revoked,[]);
  assert.equal(assets.size,1);
});

test('a zero people entry cap refuses a file without evicting', async () => {
  const server='a'.repeat(32), owner='b'.repeat(32), chat='c'.repeat(16), generation='d'.repeat(16), instance='f'.repeat(32);
  const root='/api/team/subscriptions', copy=`${root}/${server}/${owner}/${chat}/${generation}`;
  const file=`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`;
  const revoked=[], assets=new Map();
  const transport=createRemoteAssetTransport({assets,peopleByteBudget:100,peopleAssetCap:0,createObjectURL:()=>'blob:should-not-appear',revokeObjectURL:url=>revoked.push(url),channel:async request=>{
    if(request===root)return new Response(JSON.stringify(peopleCaps(server, owner, chat, generation, instance)));
    return new Response('x',{headers:{'Content-Type':'application/octet-stream'}});
  }});
  await (await transport.fetchImpl(root)).json();
  assert.equal(await transport.retryAsset(file),'');
  assert.deepEqual(revoked,[]);
  assert.equal(assets.size,0);
});

test('replacing a cached people path revokes the previous url and does not double-count bytes', async () => {
  const server='a'.repeat(32), owner='b'.repeat(32), chat='c'.repeat(16), generation='d'.repeat(16), instance='f'.repeat(32);
  const root='/api/team/subscriptions', copy=`${root}/${server}/${owner}/${chat}/${generation}`;
  const caps=peopleCaps(server, owner, chat, generation, instance);
  const first=`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`;
  const second=`${copy}/attachments/1/fedcba9876543210?copy_instance=${instance}`;
  const revoked=[], assets=new Map();
  let n=0, hide=false;
  assets.has=key=>hide&&key===first?false:Map.prototype.has.call(assets,key);
  const transport=createRemoteAssetTransport({assets,peopleByteBudget:2,peopleAssetCap:10,createObjectURL:()=>`blob:synthetic-${++n}`,revokeObjectURL:url=>revoked.push(url),channel:async request=>{
    if(request===root)return new Response(JSON.stringify(caps));
    return new Response('x',{headers:{'Content-Type':'application/octet-stream'}});
  }});
  await (await transport.fetchImpl(root)).json();
  assert.equal(await transport.retryAsset(first),'blob:synthetic-1');
  hide=true;
  assert.equal(await transport.retryAsset(first),'blob:synthetic-2');
  assert.equal(await transport.retryAsset(second),'blob:synthetic-3');
  assert.equal(transport.assetURL(first),'blob:synthetic-2');
  assert.equal(transport.assetURL(second),'blob:synthetic-3');
  assert.deepEqual(revoked,['blob:synthetic-1']);
  assert.equal(assets.size,2);
});

test('people file cache evicts the oldest entry when only the entry cap is full', async () => {
  const server='a'.repeat(32), owner='b'.repeat(32), chat='c'.repeat(16), generation='d'.repeat(16), instance='f'.repeat(32);
  const root='/api/team/subscriptions', copy=`${root}/${server}/${owner}/${chat}/${generation}`;
  const caps=peopleCaps(server, owner, chat, generation, instance);
  const ids=['0123456789abcdef','fedcba9876543210','0011223344556677'];
  const path=id=>`${copy}/attachments/1/${id}?copy_instance=${instance}`;
  const revoked=[], assets=new Map();
  let n=0;
  const transport=createRemoteAssetTransport({assets,peopleByteBudget:1000,peopleAssetCap:2,createObjectURL:()=>`blob:synthetic-${++n}`,revokeObjectURL:url=>revoked.push(url),channel:async request=>{
    if(request===root)return new Response(JSON.stringify(caps));
    return new Response('x',{headers:{'Content-Type':'application/octet-stream'}});
  }});
  await (await transport.fetchImpl(root)).json();
  assert.equal(await transport.retryAsset(path(ids[0])),'blob:synthetic-1');
  assert.equal(await transport.retryAsset(path(ids[1])),'blob:synthetic-2');
  assert.equal(await transport.retryAsset(path(ids[2])),'blob:synthetic-3');
  assert.equal(transport.assetURL(path(ids[0])),'');
  assert.equal(transport.assetURL(path(ids[1])),'blob:synthetic-2');
  assert.equal(transport.assetURL(path(ids[2])),'blob:synthetic-3');
  assert.deepEqual(revoked,['blob:synthetic-1']);
  assert.equal(assets.size,2);
});

test('people file cache evicts the oldest entry when only the byte budget is full', async () => {
  const server='a'.repeat(32), owner='b'.repeat(32), chat='c'.repeat(16), generation='d'.repeat(16), instance='f'.repeat(32);
  const root='/api/team/subscriptions', copy=`${root}/${server}/${owner}/${chat}/${generation}`;
  const caps=peopleCaps(server, owner, chat, generation, instance);
  const first=`${copy}/attachments/1/0123456789abcdef?copy_instance=${instance}`;
  const second=`${copy}/attachments/1/fedcba9876543210?copy_instance=${instance}`;
  const revoked=[], assets=new Map();
  let n=0;
  const transport=createRemoteAssetTransport({assets,peopleByteBudget:3,peopleAssetCap:100,createObjectURL:()=>`blob:synthetic-${++n}`,revokeObjectURL:url=>revoked.push(url),channel:async request=>{
    if(request===root)return new Response(JSON.stringify(caps));
    return new Response(request===first?'xx':'yy',{headers:{'Content-Type':'application/octet-stream'}});
  }});
  await (await transport.fetchImpl(root)).json();
  assert.equal(await transport.retryAsset(first),'blob:synthetic-1');
  assert.equal(await transport.retryAsset(second),'blob:synthetic-2');
  assert.equal(transport.assetURL(first),'');
  assert.equal(transport.assetURL(second),'blob:synthetic-2');
  assert.deepEqual(revoked,['blob:synthetic-1']);
  assert.equal(assets.size,1);
});
