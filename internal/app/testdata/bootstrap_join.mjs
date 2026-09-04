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
 * recorded rather than performed — but the *contents* handed to
 * createObjectURL are kept, because the one property a Node seam can still
 * check is what the loader wrote into each module: every specifier must be an
 * absolute blob: URL. Nothing relative or root-absolute resolves against a
 * blob: base, in any browser, so a module that still carries one cannot boot
 * on a device no matter how green both suites are.
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
const blobs = new Map();
const importedModules = [];
const importMaps = [];

const manifestRes = await fetch(new URL("/api/remote/bootstrap", baseURL));
if (!manifestRes.ok) {
  console.log(JSON.stringify({ error: "manifest fetch failed: " + manifestRes.status }));
  process.exit(0);
}
const manifest = await manifestRes.json();

let unresolvableSpecifiers = [];
let rewrittenSpecifiers = 0;

let out;
try {
  const result = await bootstrap({
    channel,
    manifest,
    createObjectURL: blob => {
      const url = "blob:join/" + ++blobN;
      blobs.set(url, blob);
      return url;
    },
    installImportMap: map => importMaps.push(map),
    importModule: async url => importedModules.push(url),
  });
  for (const url of Object.values(result.imports)) {
    const blob = blobs.get(url);
    if (!blob || blob.type !== "text/javascript") continue;
    const source = await blob.text();
    for (const [, spec] of source.matchAll(/(?:from|import)\s+["']([./][^"']*|blob:[^"']*)["']/g)) {
      if (spec.startsWith("blob:")) rewrittenSpecifiers += 1;
      else unresolvableSpecifiers.push(spec);
    }
  }

  out = {
    manifestEntries: manifest.entries.length,
    manifestSource: manifest.source,
    manifestEntry: manifest.entry,
    channelCalls: channelCalls.length,
    // The channel is asked for exactly the manifest's URLs and nothing else.
    channelExtras: channelCalls.filter(u => !manifest.entries.some(e => e.url === u)),
    imported: importedModules,
    objectURLs: Object.keys(result.imports).length,
    importMapsInstalled: importMaps.length,
    // Specifiers left in a minted module that a blob: base cannot resolve, and
    // the count of those rewritten to a verified object URL.
    unresolvable: unresolvableSpecifiers,
    rewritten: rewrittenSpecifiers,
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
