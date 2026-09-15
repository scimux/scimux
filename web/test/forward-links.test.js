import test from "node:test";
import assert from "node:assert/strict";
import * as links from "../js/storage.js";

function memory(seed = {}){
  const data = new Map(Object.entries(seed));
  return {
    getItem: key => data.has(key) ? data.get(key) : null,
    setItem: (key, value) => data.set(key, String(value)),
    removeItem: key => data.delete(key),
  };
}

test("pending Send-to sources survive edits/reloads and dedupe by durable address", () => {
  const storage = memory();
  const source = { node: "source", uid: "u1", segment: 2, record: 7, turnTime: "t", text: "original" };
  links.addPendingForward(storage, "dest", source);
  links.addPendingForward(storage, "dest", { ...source, text: "display changed" });
  assert.deepEqual(links.readPendingForwards(storage, "dest"), [source]);
  assert.equal(links.pendingForwardKey("dest"), "scimux-sendto-pending:dest");
});

test("clearing pending is explicit; a malformed cache safely becomes empty", () => {
  const storage = memory({ "scimux-sendto-pending:n1": "broken" });
  assert.deepEqual(links.readPendingForwards(storage, "n1"), []);
  links.addPendingForward(storage, "n1", { node: "source", turnTime: "t", text: "x" });
  links.clearPendingForwards(storage, "n1");
  assert.deepEqual(links.readPendingForwards(storage, "n1"), []);
});

test("delivery latch records the pre-send boundary and reads legacy values", () => {
  const storage = memory();
  links.writeAwaitingForward(storage, "dest", "edited prompt", 4);
  assert.deepEqual(links.readAwaitingForward(storage, "dest"), {
    text: "edited prompt", afterTurns: 4,
  });
  storage.setItem(links.awaitingForwardKey("legacy"), "old prompt");
  assert.deepEqual(links.readAwaitingForward(storage, "legacy"), {
    text: "old prompt", afterTurns: 0,
  });
  links.clearAwaitingForward(storage, "dest");
  assert.equal(links.readAwaitingForward(storage, "dest"), null);
});

test("permanent link uses the confirmed transcript address, not browser time or full text", () => {
  const source = { node: "source", uid: "u1", segment: 2, record: 7, text: "old" };
  const link = links.makeForwardLinkToTurn(
    source, "dest",
    { uid: "du", segment: 1, record: 4, time: "new", text: "edited prompt" },
  );
  assert.deepEqual(link.source, {
    node: "source", uid: "u1", segment: 2, record: 7,
  });
  assert.deepEqual(link.destination, {
    node: "dest", uid: "du", segment: 1, record: 4, turnTime: "new",
  });
  assert.equal(link.sent_at, "new");
  assert.match(link.id, /u1/);
});

test("permanent link truncates text only when the transcript has no address", () => {
  const link = links.makeForwardLinkToTurn(
    { node: "source", turnTime: "old" }, "dest",
    { role: "user", text: "x".repeat(400) },
  );
  assert.equal(link.destination.text.length, links.FORWARD_TEXT_FALLBACK_MAX);
  assert.equal(link.destination.textPrefix, true);
  assert.equal(link.destination.turnTime, undefined);
});
