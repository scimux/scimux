/* FR-41 end-to-end join driver.
 *
 * Run by at_fr40_join_test.go, never by the web suite. It imports the real
 * bootstrap.js (written out of the embedded FS by the Go test, so this
 * exercises the bytes the binary ships, not the working copy) and drives it
 * against a live httptest server running the real handlers.
 *
 * The manifest is fetched from the route rather than passed in, because the
 * point of the join is that the Go producer and the JS consumer agree with
 * nobody translating between them.
 *
 * Node has no blob: URLs and no import map, so the three platform seams are
 * recorded rather than performed. That is the same boundary the web suite
 * uses; what this driver adds is the real server on the other end of the
 * channel.
 */

const [bootstrapPath, baseURL, csrfToken] = process.argv.slice(2);

const { bootstrap } = await import(bootstrapPath);

const channelCalls = [];
const channel = async url => {
  channelCalls.push(url);
  const res = await fetch(new URL(url, baseURL));
  return {
    ok: res.ok,
    status: res.status,
    arrayBuffer: () => res.arrayBuffer(),
    text: () => res.text(),
  };
};

let blobN = 0;
const importedModules = [];
const importMaps = [];

const manifestRes = await fetch(new URL("/api/remote/bootstrap", baseURL));
if (!manifestRes.ok) {
  console.log(JSON.stringify({ error: "manifest fetch failed: " + manifestRes.status }));
  process.exit(0);
}
const manifest = await manifestRes.json();

let out;
try {
  const result = await bootstrap({
    channel,
    manifest,
    createObjectURL: () => "blob:join/" + ++blobN,
    installImportMap: map => importMaps.push(map),
    importModule: async url => importedModules.push(url),
  });
  out = {
    manifestEntries: manifest.entries.length,
    manifestSource: manifest.source,
    manifestEntry: manifest.entry,
    channelCalls: channelCalls.length,
    // The channel is asked for exactly the manifest's URLs and nothing else.
    channelExtras: channelCalls.filter(u => !manifest.entries.some(e => e.url === u)),
    imported: importedModules,
    importMapKeys: importMaps.length === 1 ? Object.keys(importMaps[0].imports).length : -1,
    stylesheets: result.stylesheets.length,
    assets: result.assets.length,
    entry: result.entry,
    indexLength: result.index.length,
    indexHasToken: csrfToken.length > 0 && result.index.includes(csrfToken),
    indexHasPlaceholder: result.index.includes("__SCIMUX_CSRF__"),
  };
} catch (err) {
  out = { error: String(err && err.name) + ": " + String(err && err.message) };
}

console.log(JSON.stringify(out));
