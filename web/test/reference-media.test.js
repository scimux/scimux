import test from "node:test";
import assert from "node:assert/strict";

import {
  renderReferenceMedia,
  mediaTextAndTiles,
  validReferenceMedia,
  createLegacyMediaResolver,
  legacyMediaTextAndTiles,
} from "../js/reference-media.js";
import { bookmarkListHTML } from "../js/bookmarks.js";
import { inboxItemHTML, referenceHTML, buildBookmarkSnapshot } from "../js/notes.js";

const esc = s => String(s ?? "").replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll('"', "&quot;");

const media = {
  version: 1,
  capture_id: "a".repeat(64),
  items: [
    { id: "0", key: "asset:a_1", name: "one.png", state: "ready" },
    { id: "1", key: "asset:a_2", name: "two.jpg", state: "unavailable" },
  ],
};

test("durable media preserves surrounding text and image-only records", () => {
  const mixed = mediaTextAndTiles("before ![one](scimux-asset:a_1) after", media, { esc, assetURL: x => `remote:${x}` });
  assert.equal(mixed.clean, "before  after");
  assert.match(mixed.html, /remote:\/api\/reference-media\/a{64}\/assets\/0/);
  assert.match(mixed.html, /two\.jpg \(unavailable\)/);

  const imageOnly = mediaTextAndTiles("![one](scimux-asset:a_1)", media, { esc, assetURL: x => x });
  assert.equal(imageOnly.clean, "");
  assert.match(imageOnly.html, /data-asset-preview/);
});

test("ordinary asset links become tiles only when their canonical descriptor is an image", () => {
  const mixed = mediaTextAndTiles("[one](scimux-asset:a_1) [manual](scimux-asset:file_1)", media, { esc, assetURL:x=>x });
  assert.equal(mixed.clean, "[manual](scimux-asset:file_1)");
  assert.match(mixed.html, /one\.png/);
});

test("the same safe tile renderer serves bookmark, inbox, and reference surfaces", () => {
  const html = renderReferenceMedia(media, { esc, assetURL: x => x });
  assert.match(html, /class="attrow reference-media"/);
  assert.doesNotMatch(html, /javascript:|data:|file:/);
  assert.match(html, /one\.png/);
  assert.match(html, /unavailable/);
});

test("malformed descriptors are inert", () => {
  const hostile = { version: 1, capture_id: "../bad", items: [{ id: "0", key: "asset:x", name: "<svg onload=x>", state: "ready" }] };
  const html = renderReferenceMedia(hostile, { esc, assetURL: x => x });
  assert.match(html, /unavailable/);
  assert.doesNotMatch(html, /onload=/);
});

test("descriptor validation and default escaping fail closed", () => {
  assert.equal(validReferenceMedia(null), false);
  assert.equal(validReferenceMedia({...media, items:Array.from({length:33}, (_, i) => ({id:String(i),key:`asset:a_${i}`,name:"x.png",state:"ready"}))}), false);
  const html = renderReferenceMedia({version:1,capture_id:"f".repeat(64),items:[{id:"0",key:"asset:x",name:"<&\"'",state:"unavailable"}]});
  assert.match(html, /&lt;&amp;&quot;&#39;/);
  assert.match(renderReferenceMedia({version:1,capture_id:"f".repeat(64),items:[{id:"0",key:"asset:x",name:"x.png",state:"ready"}]}), /href="\/api\/reference-media/);
  assert.throws(() => createLegacyMediaResolver({}), /requires api/);
});

test("bookmark pane, Notes inbox, and embedded reference render durable tiles", () => {
  const nt = { t:"now", text:"before ![one](scimux-asset:a_1) after", media };
  const deps = { esc, md:s=>s, fmtWhen:s=>s, assetURL:p=>`remote:${p}`, nodeById:()=>null };
  const pane = bookmarkListHTML({ list:[nt], bookmarkTab:"GENERAL", ...deps });
  const inbox = inboxItemHTML(nt, "red", deps);
  const ref = referenceHTML({ id:"r", snapshot:{ text:nt.text, media } }, deps);
  for (const html of [pane, inbox, ref]) {
    assert.match(html, /remote:\/api\/reference-media\/a{64}\/assets\/0/);
    assert.match(html, /before  after/);
  }
});

test("bookmark snapshots deep-copy media descriptors", () => {
  const nt = { t:"now", text:"x", media };
  const snap = buildBookmarkSnapshot(nt);
  nt.media.items[0].name = "changed.png";
  assert.equal(snap.media.items[0].name, "one.png");
});

test("legacy lookup is exact, coalesced, bounded, and read-only", async () => {
  let calls = 0;
  let finish;
  const resolver = createLegacyMediaResolver({ maxEntries:2, api: async () => {
    calls++;
    return new Promise(resolve => { finish = resolve; });
  } });
  const record = { uid:"u", segment:0, record:0, text:"![one](scimux-asset:a_1)" };
  assert.equal(resolver.view(record).state, "pending");
  assert.equal(resolver.view(record).state, "pending");
  assert.equal(calls, 1);
  finish({ uid:"u", node:"live", anchor:0, turns:[{uid:"u",segment:0,record:0}], assets:{a_1:{name:"one.png"}} });
  await resolver.settled();
  const rendered = legacyMediaTextAndTiles(record, resolver, { esc, assetURL:p=>`remote:${p}` });
  assert.equal(rendered.clean, "");
  assert.match(rendered.html, /remote:\/api\/nodes\/live\/assets\/a_1/);
  resolver.destroy();
});

test("legacy preview accepts wire-omitted zeros but still checks the exact address", async () => {
  for (const ordinal of [0, 3]) {
    const resolver = createLegacyMediaResolver({api:async () => ({
      uid:"synthetic", node:"live", anchor:0,
      turns:[{uid:"synthetic", ...(ordinal ? {record:ordinal} : {})}], assets:{},
    })});
    const record = {uid:"synthetic",segment:0,record:ordinal};
    resolver.view(record);
    await resolver.settled();
    assert.equal(resolver.view(record).state, "ready");
    resolver.destroy();
  }
});

test("legacy lookup rejects missing addresses and preview fallback mismatches", async () => {
  const resolver = createLegacyMediaResolver({ api: async () => ({
    uid:"u", node:"reused", anchor:0, turns:[{uid:"u",segment:0,record:9}], assets:{a_1:{name:"wrong.png"}},
  }) });
  assert.equal(resolver.view({uid:"u",text:"![x](scimux-asset:a_1)"}).state, "unavailable");
  const record = {uid:"u",segment:0,record:0,text:"![x](scimux-asset:a_1)"};
  resolver.view(record); await resolver.settled();
  const out = legacyMediaTextAndTiles(record, resolver, {esc,assetURL:p=>p});
  assert.match(out.html, /unavailable/);
  assert.doesNotMatch(out.html, /wrong\.png|reused/);
});

test("legacy lookup renders ordinary raster asset links and preserves ordinary files", async () => {
  const resolver = createLegacyMediaResolver({ api: async () => ({
    uid:"u", node:"live", anchor:0, turns:[{uid:"u",segment:0,record:0}],
    assets:{a_1:{name:"one.png"}, file_1:{name:"manual.pdf"}},
  }) });
  const record = {uid:"u",segment:0,record:0,text:"[one](scimux-asset:a_1) [manual](scimux-asset:file_1)"};
  resolver.view(record); await resolver.settled();
  const out = legacyMediaTextAndTiles(record, resolver, {esc,assetURL:p=>p});
  assert.match(out.html, /assets\/a_1/);
  assert.equal(out.clean, "[manual](scimux-asset:file_1)");
  assert.match(legacyMediaTextAndTiles(record, resolver).html, /href="\/api\/nodes\/live\/assets\/a_1/);
});

test("legacy cache keeps settled entries, bounds unseen entries, and handles network failure", async () => {
  let calls = 0;
  const resolver = createLegacyMediaResolver({maxEntries:1, api:async path => { calls++; return ({
    uid:path.includes("uid=u1") ? "u1" : "u2", node:"live", anchor:0,
    turns:[{uid:path.includes("uid=u1") ? "u1" : "u2",segment:0,record:0}], assets:{},
  }); }});
  resolver.view({uid:"u1",segment:0,record:0}); await resolver.settled();
  assert.equal(resolver.view({uid:"u2",segment:0,record:0}).state, "unavailable");
  assert.equal(resolver.size(), 1);
  assert.equal(calls, 1);

  let reject;
  const pending = createLegacyMediaResolver({maxEntries:1, AbortControllerImpl:null, api:async () => new Promise((_r, rej) => { reject=rej; })});
  pending.view({uid:"a",segment:0,record:0});
  assert.equal(pending.view({uid:"b",segment:0,record:0}).state, "unavailable");
  reject(new Error("offline")); await pending.settled();
  assert.equal(pending.view({uid:"a",segment:0,record:0}).state, "unavailable");
  pending.destroy();
});

test("legacy controller settles when visible records exceed cache capacity", async () => {
  const records = [0,1,2].map(record => ({uid:"u",segment:0,record,text:`![x](scimux-asset:a_${record})`}));
  let calls = 0, renders = 0, resolver;
  const render = () => {
    renders++;
    for (const record of records) legacyMediaTextAndTiles(record, resolver, {esc,assetURL:p=>p});
  };
  resolver = createLegacyMediaResolver({maxEntries:2,onUpdate:render,api:async path => {
    calls++;
    const record=Number(new URL(`http://local${path}`).searchParams.get("rec"));
    if (record === 1) return {uid:"u",node:"live",anchor:0,turns:[{uid:"u",segment:0,record:99}],assets:{}};
    return {uid:"u",node:"live",anchor:0,turns:[{uid:"u",segment:0,record}],assets:{[`a_${record}`]:{name:"x.png"}}};
  }});
  render(); await resolver.settled();
  for (let i=0;i<4;i++){ render(); await resolver.settled(); }
  assert.equal(calls, 2);
  assert.equal(resolver.size(), 2);
  assert.ok(renders >= 3);
  resolver.destroy();
});
