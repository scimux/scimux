/* P1: Ready dismiss wiring. Pure helpers live in map-model.test.js; this
 * file locks the shell/map seams so opening chat acks and wall-select does not. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const appSrc = readFileSync(join(__dirname, "../js/app.js"), "utf8");
const mapSrc = readFileSync(join(__dirname, "../js/map.js"), "utf8");
const cardsSrc = readFileSync(join(__dirname, "../js/cards.js"), "utf8");

test("app.js loads and persists Ready seen via injected helpers", () => {
  assert.match(appSrc, /loadReadySeen/);
  assert.match(appSrc, /saveReadySeen/);
  assert.match(appSrc, /ackReadySeen/);
  assert.match(appSrc, /pruneReadySeen/);
  assert.match(appSrc, /READY_SEEN_KEY|loadReadySeen\(/);
});

test("select() acks the opened node", () => {
  const start = appSrc.indexOf("function select(id, how){");
  assert.ok(start >= 0, "select() is a named function");
  assert.match(appSrc.slice(start, start + 1400), /ackReadySeen/,
    "opening current chat records seen");
});

test("state publish prunes seen and acks the already-open chat", () => {
  assert.match(appSrc, /publishState:\s*st\s*=>\s*\{[\s\S]*?pruneReadySeen[\s\S]*?\}/);
  assert.match(appSrc, /publishState:[\s\S]*?ackReadySeen/);
});

test("cards and map receive readySeen; map also receives sel", () => {
  assert.match(cardsSrc, /g\("readySeen"/);
  assert.match(mapSrc, /g\("readySeen"/);
  assert.match(appSrc, /readySeen:\s*\(\)\s*=>\s*readySeen/);
  assert.match(appSrc, /createMapFeature\([\s\S]*?sel:\s*\(\)\s*=>\s*sel/);
});

test("map setMapSel does not call selectNode (wall select is not an ack)", () => {
  const fn = mapSrc.match(/function setMapSel\([\s\S]*?\n  \}/);
  assert.ok(fn, "setMapSel is present");
  assert.doesNotMatch(fn[0], /selectNode/,
    "full-screen station select must not open chat or ack Ready");
});
