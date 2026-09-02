/* Pure lane-picker HTML and form-choice decisions. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  NEW_LANE_VALUE,
  laneOptionsHTML,
  laneSelectHTML,
  readLaneChoiceFromValues,
} from "../js/lane-picker.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(__dirname, "../js/lane-picker.js"), "utf8");
const appSrc = readFileSync(join(__dirname, "../js/app.js"), "utf8");
const lanesSrc = readFileSync(join(__dirname, "../js/lanes.js"), "utf8");

test("lane-picker.js is pure and lanes.js stays HTML-free", () => {
  assert.doesNotMatch(src, /\bdocument\b|\bwindow\b|\bfetch\b/);
  assert.doesNotMatch(lanesSrc, /\blaneOptionsHTML\b|\blaneSelectHTML\b|\breadLaneChoiceFromValues\b/);
  assert.match(appSrc, /from "\.\/lane-picker\.js"/);
  assert.equal(NEW_LANE_VALUE, "__new");
});

test("laneOptionsHTML: unset, missing selected, lanes, and New lane option", () => {
  const lanes = [{ id: "l1", name: "Alpha" }, { id: "l2", name: "Beta" }];
  const html = laneOptionsHTML("l1", lanes);
  assert.match(html, /<option value="" >/);
  assert.match(html, /value="l1" selected>Alpha</);
  assert.match(html, /value="l2"[^>]*>Beta</);
  assert.match(html, new RegExp(`value="${NEW_LANE_VALUE}">New lane`));
  assert.doesNotMatch(html, /value="l9"/);

  const missing = laneOptionsHTML("gone", lanes);
  assert.match(missing, /value="gone" selected>gone</);

  const noUnset = laneOptionsHTML("l1", lanes, { includeUnset: false });
  assert.doesNotMatch(noUnset, /<option value="" /);

  const disabled = laneOptionsHTML("", lanes, { disabled: true });
  assert.doesNotMatch(disabled, /New lane/);
});

test("laneSelectHTML wraps options and respects disabled", () => {
  const html = laneSelectHTML("l1", [{ id: "l1", name: "A" }]);
  assert.match(html, /^<select data-lane-select >/);
  assert.match(html, /selected>A</);
  const dis = laneSelectHTML("", [], { disabled: true });
  assert.match(dis, /disabled/);
});

test("readLaneChoiceFromValues: existing id, new lane, and empty-name error", () => {
  const makeLane = (name) => ({ id: "new-" + name, name, color: "#000" });
  assert.deepEqual(readLaneChoiceFromValues("l1", ""), { laneID: "l1" });
  assert.deepEqual(readLaneChoiceFromValues("", "ignored"), { laneID: "" });
  assert.deepEqual(
    readLaneChoiceFromValues(NEW_LANE_VALUE, "  "),
    { error: "Enter a lane name." },
  );
  assert.deepEqual(
    readLaneChoiceFromValues(NEW_LANE_VALUE, "  Trip  ", { makeLane }),
    { laneID: "new-Trip", lane: { id: "new-Trip", name: "Trip", color: "#000" } },
  );
});
