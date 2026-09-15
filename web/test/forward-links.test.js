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

test("successful send creates stable links to the actual destination text", () => {
  const source = { node: "source", uid: "u1", segment: 2, record: 7, text: "old" };
  const link = links.makeForwardLink(source, "dest", "edited before send", "2026-09-15T12:00:00Z");
  assert.deepEqual(link.source, source);
  assert.deepEqual(link.destination, {
    node: "dest", turnTime: "2026-09-15T12:00:00Z", text: "edited before send",
  });
  assert.match(link.id, /u1/);
});

test("deferred initial delivery has a separate expected-turn latch", () => {
  const storage = memory();
  links.writeAwaitingForward(storage, "dest", "edited prompt");
  assert.equal(links.readAwaitingForward(storage, "dest"), "edited prompt");
  const link = links.makeForwardLinkToTurn(
    { node: "source", turnTime: "old" }, "dest",
    { uid: "du", segment: 1, record: 4, time: "new", text: "edited prompt" },
  );
  assert.deepEqual(link.destination, {
    node: "dest", uid: "du", segment: 1, record: 4, turnTime: "new", text: "edited prompt",
  });
  links.clearAwaitingForward(storage, "dest");
  assert.equal(links.readAwaitingForward(storage, "dest"), "");
});
