/* Gate C, web half — a test must exercise code the browser actually loads.
 *
 * This arc has now turned up the same defect five times: the primitive is
 * built and tested, and no product path reaches it. FR-16 on the in-process
 * harness path; the pairing SAS; remote.Config.TunnelHandler, set by nothing;
 * AcceptPairingOffer with no product caller; and — the reason this file
 * exists — a whole pairing UI that lives only in web/test/, so five ATs pass
 * against an implementation that ships in no build.
 *
 * The rule is mechanical: a *.test.js may import helpers from the test
 * directory, but an implementation it asserts over must come from web/js/,
 * which is what web/embed.go serves. A test-directory module that exports
 * product behaviour is the failure this catches, and it catches it at the
 * import, before anyone reads the assertions and believes them.
 */
import test from "node:test";
import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));

// Helpers are allowed to live beside the tests. Each entry is a module that
// wires up fixtures or a DOM stub and asserts nothing about product
// behaviour of its own. Adding a name here is a deliberate claim that the
// module is scaffolding — not a way to park an implementation.
const ALLOWED_TEST_LOCAL = new Set(["./s2-helpers.js"]);

// The other kind of waiver, matching the Go half's reachOpenFindings: a
// module that ought to be served and is not yet. These are debt, written
// down rather than found a second time. Deleting an entry — by moving the
// module under web/js/ and importing it from there — is the fix.
const KNOWN_UNREACHABLE = new Map([
  [
    "./pairing-ui.js",
    "S7 parked the pairing UI in web/test/ deliberately, to keep the served " +
      "inventory and the FR-40 ratchet untouched while the computer half was " +
      "built. Its own header says so. Five acceptance tests (AT-FR-38-a..c, " +
      "AT-NFR-12-a) therefore pass against an implementation that ships in no " +
      "build. S8 moves it under web/js/ and wires it into index.html; this " +
      "entry goes with it.",
  ],
]);

function testFiles() {
  return readdirSync(here)
    .filter((n) => n.endsWith(".test.js"))
    .sort();
}

function importsOf(source) {
  const out = [];
  const re = /\bfrom\s+["']([^"']+)["']/g;
  let m;
  while ((m = re.exec(source)) !== null) out.push(m[1]);
  return out;
}

function relativeImports() {
  const out = [];
  for (const file of testFiles()) {
    const src = readFileSync(join(here, file), "utf8");
    for (const spec of importsOf(src)) {
      // node: builtins and bare specifiers are not ours to police.
      if (spec.startsWith(".")) out.push({ file, spec });
    }
  }
  return out;
}

// The anti-vacuity assertion, same role as the Go half's: a scan that reached
// nothing would report no offenders forever.
test("the reachability scan reaches the test directory", () => {
  const files = testFiles();
  assert.ok(
    files.length > 20,
    `found only ${files.length} *.test.js files; the scan is not reading the ` +
      "test directory, so the rule below would pass vacuously",
  );
  const imports = relativeImports();
  assert.ok(
    imports.length > 20,
    `found only ${imports.length} relative imports across ${files.length} ` +
      "test files; the import parse is not working",
  );
  assert.ok(
    imports.some((i) => i.spec.startsWith("../js/")),
    "no test imports ../js/ at all, which cannot be true — the parse is broken",
  );
});

test("web tests import served code, not test-directory implementations", () => {
  const offenders = [];
  for (const { file, spec } of relativeImports()) {
    if (spec.startsWith("../js/")) continue; // served by web/embed.go
    if (ALLOWED_TEST_LOCAL.has(spec)) continue;
    if (KNOWN_UNREACHABLE.has(spec)) continue;
    offenders.push(`${file} imports ${spec}`);
  }
  assert.deepEqual(
    offenders,
    [],
    "these tests assert over code the browser never loads, so they cannot go " +
      "red when the product is broken:\n  " +
      offenders.join("\n  ") +
      "\nMove the implementation under web/js/ and import it from there; or " +
      "name it in ALLOWED_TEST_LOCAL if it really is scaffolding, or in " +
      "KNOWN_UNREACHABLE with the reason if it ought to be served and is not " +
      "yet.",
  );
});

// Load-bearing in the other direction, matching the Go half: a waiver kept
// past its cause is permission nobody reviewed, waiting for the next module
// that happens to need it.
test("reachability waivers still excuse something", () => {
  const specs = new Set(relativeImports().map((i) => i.spec));
  const stale = [];
  for (const name of ALLOWED_TEST_LOCAL) {
    if (!specs.has(name)) stale.push(`${name} (ALLOWED_TEST_LOCAL)`);
  }
  for (const name of KNOWN_UNREACHABLE.keys()) {
    if (!specs.has(name)) stale.push(`${name} (KNOWN_UNREACHABLE)`);
  }
  assert.deepEqual(
    stale,
    [],
    "these waivers name modules no test imports any more:\n  " +
      stale.join("\n  ") +
      "\nDelete them.",
  );
});
