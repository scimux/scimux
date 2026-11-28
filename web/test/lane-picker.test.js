/* Pure lane-picker HTML and form-choice decisions. */
import test from "node:test";
import assert from "node:assert/strict";
import {
  NEW_LANE_VALUE,
  laneOptionsHTML,
  laneSelectHTML,
  readLaneChoiceFromValues,
  laneShareNotice,
  laneShareFacts,
  paintLaneShareNotice,
} from "../js/lane-picker.js";

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

test("laneOptionsHTML escapes authored and missing lane values", () => {
  const html = laneOptionsHTML("missing<&", [{ id: `lane\"`, name: "R&D <work>" }]);
  assert.match(html, /value="missing&lt;&amp;" selected>missing&lt;&amp;</);
  assert.match(html, /value="lane&quot;"[^>]*>R&amp;D &lt;work&gt;</);
  assert.doesNotMatch(html, /missing<&|R&D <work>|value="lane\""/);
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

test("laneShareFacts counts open chats and archived ones, and is empty without a lane", () => {
  const nodes = [
    { id: "a", lane_id: "north" },
    null,
    { id: "b", lane_id: "north" },
    { id: "c", lane_id: "south" },
    { lane_id: "north" },
  ];
  assert.deepEqual(
    laneShareFacts("north", { id: "north", name: "North" }, "#AABBCC", nodes, ["b"]),
    { name: "North", color: "#AABBCC", open: 2, archived: ["b"] },
  );
  assert.equal(laneShareFacts("north", null, "#AABBCC", nodes, ["b"]), null);
  assert.deepEqual(
    laneShareFacts("north", { id: "north" }, "", null, "nope"),
    { name: "north", color: "", open: 0, archived: [] },
  );
});

test("laneShareNotice names one person, many people, and stays quiet otherwise", () => {
  const sentence = (n, server) =>
    "This lane is shared with " + (n === 1 ? "1 person" : n + " people") +
    " on " + server + ". This chat will be shared as soon as it starts.";
  const active = (n) => ({ state: "active", members: Array.from({ length: n }, (_, i) => ({ seat: "s" + i })) });
  assert.equal(laneShareNotice(active(1), "Lab"), sentence(1, "Lab"));
  assert.equal(laneShareNotice(active(3), "Dept"), sentence(3, "Dept"));
  assert.equal(laneShareNotice(active(0), "Lab"), sentence(0, "Lab"));
  assert.equal(laneShareNotice({ state: "active" }, "Lab"), sentence(0, "Lab"));
  assert.equal(laneShareNotice(null, "Lab"), "");
  assert.equal(laneShareNotice({ state: "stopping", members: [{ seat: "a" }] }, "Lab"), "");
  assert.equal(laneShareNotice({ state: "lost", members: [{ seat: "a" }] }, "Lab"), "");
});

test("paintLaneShareNotice fills the element next to the select and hides an empty one", () => {
  const note = { textContent: "old", hidden: false };
  const pick = { querySelector: (sel) => sel === "[data-lane-share]" ? note : null };
  assert.equal(paintLaneShareNotice(pick, "This lane is shared with 1 person on Lab. This chat will be shared as soon as it starts."), true);
  assert.equal(note.textContent, "This lane is shared with 1 person on Lab. This chat will be shared as soon as it starts.");
  assert.equal(note.hidden, false);
  assert.equal(paintLaneShareNotice(pick, ""), true);
  assert.equal(note.hidden, true);
  assert.equal(paintLaneShareNotice({ querySelector: () => null }, "x"), false);
  assert.equal(paintLaneShareNotice(null, "x"), false);
  assert.equal(paintLaneShareNotice({}, "x"), false);
});
