import test from "node:test";
import assert from "node:assert/strict";

import { createBookmarkCapture } from "../js/bookmark-capture.js";

const turn = {
  uid: "u1", segment: 0, record: 0, role: "assistant", agent: "synthetic",
  text: "![chart](scimux-asset:a_1)", prov: { source: ["synthetic"] },
};

test("capture waits for durable publication and coalesces a pending duplicate", async () => {
  let resolve;
  let calls = 0;
  const api = async () => { calls++; return new Promise(r => { resolve = r; }); };
  const capture = createBookmarkCapture({ api });
  const bookmark = { t: "now", text: turn.text };
  const a = capture.capture(bookmark, turn);
  const b = capture.capture({ t: "later", text: turn.text }, turn);
  assert.equal(calls, 1);
  resolve({ text: "durable text", media: { version: 1, capture_id: "b".repeat(64), items: [{id:"0",key:"asset:a_1",name:"chart.png",state:"ready"}] } });
  const [one, two] = await Promise.all([a, b]);
  assert.equal(one.text, "durable text");
  assert.equal(two.media.capture_id, "b".repeat(64));
  assert.notStrictEqual(one, two);
});

test("text-only bookmarks avoid capture and provenance is deep copied", async () => {
  let calls = 0;
  const capture = createBookmarkCapture({ api: async () => { calls++; } });
  const source = { ...turn, text: "ordinary text" };
  const bookmark = { t: "now", text: source.text, role: source.role, agent: source.agent, prov: source.prov };
  const got = await capture.capture(bookmark, source);
  source.prov.source.push("later");
  assert.equal(calls, 0);
  assert.deepEqual(got.prov, { source: ["synthetic"] });
});

test("ordinary managed asset markers wait for server-side image classification", async () => {
  let calls = 0;
  const capture = createBookmarkCapture({ api: async () => {
    calls++;
    return { text:"[chart](scimux-asset:a_1)", media:{version:1,capture_id:"e".repeat(64),items:[{id:"0",key:"asset:a_1",name:"chart.png",state:"ready"}]}};
  } });
  const source = {...turn, text:"[chart](scimux-asset:a_1)"};
  const got = await capture.capture({t:"now",text:source.text}, source);
  assert.equal(calls, 1);
  assert.equal(got.media.items[0].name, "chart.png");
});

test("a failed capture remains retryable", async () => {
  let attempts = 0;
  const capture = createBookmarkCapture({ api: async () => {
    attempts++;
    if (attempts === 1) throw new Error("budget");
    return { text: turn.text, media: { version: 1, capture_id: "c".repeat(64), items: [{id:"0",key:"asset:a_1",name:"chart.png",state:"ready"}] } };
  } });
  await assert.rejects(capture.capture({ t: "one", text: turn.text }, turn), /budget/);
  assert.equal((await capture.capture({ t: "two", text: turn.text }, turn)).media.capture_id, "c".repeat(64));
});

test("teardown invalidates a late response", async () => {
  let finish;
  const capture = createBookmarkCapture({ api: async () => new Promise(resolve => { finish = resolve; }) });
  const pending = capture.capture({t:"one",text:turn.text}, turn);
  capture.clear();
  finish({text:turn.text,media:{version:1,capture_id:"d".repeat(64),items:[]}});
  await assert.rejects(pending, /cancelled/);
});

test("capture validates construction and exact source addresses", () => {
  assert.throws(() => createBookmarkCapture({}), /requires api/);
  const capture = createBookmarkCapture({api:async()=>({})});
  assert.throws(() => capture.capture({text:"![x](scimux-asset:a_1)"}, null), /no durable source/);
  assert.throws(() => capture.capture({text:"![x](scimux-asset:a_1)"}, {uid:"u",segment:-1,record:0}), /no durable source/);
  assert.deepEqual(capture.capture(undefined, undefined), {});
});
