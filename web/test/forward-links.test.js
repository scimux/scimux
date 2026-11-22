import test from "node:test";
import assert from "node:assert/strict";
import * as links from "../js/storage.js";

function memory(seed = {}){
  const data = new Map(Object.entries(seed));
  return {
    getItem: key => data.has(key) ? data.get(key) : null,
    setItem: (key, value) => data.set(key, String(value)),
    removeItem: key => data.delete(key),
    keys: () => [...data.keys()],
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

test("delivery latch records the pre-send boundary and stamps its write", () => {
  const storage = memory();
  links.writeAwaitingForward(storage, "dest", "edited prompt", 4, 5_000);
  assert.deepEqual(links.readAwaitingForward(storage, "dest", 5_001), {
    text: "edited prompt", afterTurns: 4, at: 5_000,
  });
  links.clearAwaitingForward(storage, "dest");
  assert.equal(links.readAwaitingForward(storage, "dest", 5_001), null);
});

/* A latch only clears when its exact text lands as a destination turn. Any
   divergence the matcher cannot bridge would otherwise hold the node's
   Send-to state forever, because both the cancel guard and the overwrite
   guard read "a latch exists" as "a delivery is still in flight". */
test("a latch that never confirms expires instead of wedging the node", () => {
  const storage = memory();
  links.writeAwaitingForward(storage, "dest", "never lands", 0, 5_000);
  const late = 5_000 + links.AWAITING_FORWARD_TTL_MS + 1;
  assert.equal(links.readAwaitingForward(storage, "dest", late), null);
  assert.equal(storage.getItem(links.awaitingForwardKey("dest")), null,
    "the dead latch is removed, not merely hidden from this read");
});

test("unstamped latches from before the TTL cannot wedge a node", () => {
  const storage = memory({
    [links.awaitingForwardKey("legacy")]: "old prompt",
    [links.awaitingForwardKey("v1")]: JSON.stringify({ text: "old prompt", afterTurns: 2 }),
  });
  assert.equal(links.readAwaitingForward(storage, "legacy", 5_000), null);
  assert.equal(links.readAwaitingForward(storage, "v1", 5_000), null);
  assert.equal(storage.getItem(links.awaitingForwardKey("v1")), null);
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

test('prompt source identities distinguish local records and full shared generations', () => {
  const local={uid:'u',segment:2,record:7};
  const team={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:4};
  assert.notEqual(links.promptSourceKey(local),links.promptSourceKey({...local,record:8}));
  assert.notEqual(links.promptSourceKey(team),links.promptSourceKey({...team,generation:'g2'}));
  assert.equal(links.promptSourceKey({...team,seq:0}),'');
  const store=memory(); links.addPromptSource(store,'n1',local); links.addPromptSource(store,'n1',team);
  links.addPromptSource(store,'n1',{...team});
  assert.deepEqual(links.readPromptSources(store,'n1'),[local,team]);
  links.clearPromptSources(store,'n1'); assert.deepEqual(links.readPromptSources(store,'n1'),[]);
});

test('send-to retains an exact shared source across drafts and confirmed links', () => {
  const source={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:4},other={...source,seq:5},storage=memory();
  links.addPendingForward(storage,'dest',source);links.addPendingForward(storage,'dest',{...source});links.addPendingForward(storage,'dest',other);
  assert.deepEqual(links.readPendingForwards(storage,'dest'),[source,other]);
  const link=links.makeForwardLinkToTurn(source,'dest',{uid:'u',segment:0,record:1,time:'then'});
  assert.deepEqual(link.source,source);assert.notEqual(links.sourceAddressKey(source),links.sourceAddressKey(other));
});

test('an incomplete shared source cannot replace a pending durable source', () => {
  const storage=memory(),source={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:4};
  links.addPendingForward(storage,'dest',source);
  assert.deepEqual(links.addPendingForward(storage,'dest',{...source,generation:''}),[source]);
  assert.deepEqual(links.addPendingForward(storage,'dest',null),[source]);
});

test("bindDraftIdentity adopts slug values when no owner record exists", () => {
  const storage = memory({
    "scimux-draft:n1": "kept draft",
    "scimux-prompt-sources:n1": '[{"uid":"kept"}]',
    "scimux-sendto-pending:n1": '[{"node":"s"}]',
  });
  assert.equal(links.bindDraftIdentity(storage, "n1", "fresh"), true);
  assert.equal(storage.getItem("scimux-draft:n1"), "kept draft");
  assert.equal(storage.getItem("scimux-prompt-sources:n1"), '[{"uid":"kept"}]');
  assert.equal(storage.getItem("scimux-sendto-pending:n1"), '[{"node":"s"}]');
  assert.equal(storage.getItem("scimux-draft-owner:n1"), "fresh");
  assert.equal(storage.getItem("scimux-retired-index:n1"), null);
});

test("bindDraftIdentity drops every slug value when the owner record is a different chat", () => {
  const storage = memory({
    "scimux-draft-owner:n1": "old",
    "scimux-draft:n1": "old text",
    "scimux-people-draft:n1": "old discussion",
    "scimux-prompt-sources:n1": '[{"uid":"old"}]',
    "scimux-prompt-flights:n1": "[]",
    "scimux-sendto-pending:n1": "[]",
    "scimux-sendto-awaiting:n1": "[]",
  });
  assert.equal(links.bindDraftIdentity(storage, "n1", "new"), true);
  for (const prefix of ["scimux-draft:", "scimux-people-draft:", "scimux-prompt-sources:", "scimux-prompt-flights:", "scimux-sendto-pending:", "scimux-sendto-awaiting:"]) {
    assert.equal(storage.getItem(prefix + "n1"), null, prefix);
  }
  assert.equal(storage.getItem("scimux-draft-owner:n1"), "new");
  assert.equal(storage.getItem("scimux-retired-index:n1"), null);
  assert.equal(storage.getItem("scimux-retired-drafts:n1:old"), null);
  assert.equal(storage.getItem("scimux-retired-drafts:n1:unknown"), null);
});

test("bindDraftIdentity restores the last unknown archive into absent live keys and then drops the archive", () => {
  const storage = memory({
    "scimux-draft-owner:n1": "current",
    "scimux-draft:n1": "already live",
    "scimux-retired-index:n1": JSON.stringify(["unknown", "other"]),
    "scimux-retired-drafts:n1:unknown": JSON.stringify([
      {"scimux-draft:": "first draft"},
      {
        "scimux-draft:": "last draft",
        "scimux-prompt-sources:": '[{"uid":"last"}]',
        "scimux-sendto-pending:": '[{"node":"s"}]',
        "scimux-prompt-flights:": 12,
        "not-a-prefix": "skip",
      },
    ]),
    "scimux-retired-drafts:n1:other": JSON.stringify([{"scimux-draft:": "known owner", "scimux-people-draft:": "neither"}]),
  });
  assert.equal(links.bindDraftIdentity(storage, "n1", "current"), true);
  assert.equal(storage.getItem("scimux-prompt-sources:n1"), '[{"uid":"last"}]');
  assert.equal(storage.getItem("scimux-sendto-pending:n1"), '[{"node":"s"}]');
  assert.equal(storage.getItem("scimux-draft:n1"), "already live");
  assert.equal(storage.getItem("scimux-prompt-flights:n1"), null);
  assert.equal(storage.getItem("scimux-people-draft:n1"), null);
  assert.equal(storage.getItem("not-a-prefixn1"), null);
  assert.equal(storage.getItem("scimux-retired-index:n1"), null);
  assert.equal(storage.getItem("scimux-retired-drafts:n1:unknown"), null);
  assert.equal(storage.getItem("scimux-retired-drafts:n1:other"), null);
  assert.equal(links.bindDraftIdentity(storage, "n1", "current"), true);
  assert.equal(storage.getItem("scimux-prompt-sources:n1"), '[{"uid":"last"}]');

  const adopted = memory({
    "scimux-retired-index:n1": JSON.stringify(["unknown"]),
    "scimux-retired-drafts:n1:unknown": JSON.stringify([{"scimux-draft:": "from archive"}]),
  });
  assert.equal(links.bindDraftIdentity(adopted, "n1", "fresh"), true);
  assert.equal(adopted.getItem("scimux-draft:n1"), "from archive");
  assert.equal(adopted.getItem("scimux-draft-owner:n1"), "fresh");
  assert.equal(adopted.getItem("scimux-retired-index:n1"), null);

  const dropped = memory({
    "scimux-draft-owner:n1": "other",
    "scimux-retired-index:n1": JSON.stringify(["unknown"]),
    "scimux-retired-drafts:n1:unknown": JSON.stringify([{"scimux-draft:": "restored then dropped"}]),
  });
  assert.equal(links.bindDraftIdentity(dropped, "n1", "new"), true);
  assert.equal(dropped.getItem("scimux-draft:n1"), null);
  assert.equal(dropped.getItem("scimux-draft-owner:n1"), "new");
  assert.equal(dropped.getItem("scimux-retired-index:n1"), null);
  assert.equal(dropped.getItem("scimux-retired-drafts:n1:unknown"), null);
});

test("bindDraftIdentity drops a known-owner archive without restoring it", () => {
  const storage = memory({
    "scimux-draft-owner:n1": "current",
    "scimux-retired-index:n1": JSON.stringify(["old"]),
    "scimux-retired-drafts:n1:old": JSON.stringify([{"scimux-draft:": "do not restore", "scimux-people-draft:": "neither"}]),
    "scimux-retired-flight:n1\0old": "leave this",
    "scimux-retired-visible:n1\0old": "leave visible",
  });
  assert.equal(links.bindDraftIdentity(storage, "n1", "current"), true);
  assert.equal(storage.getItem("scimux-draft:n1"), null);
  assert.equal(storage.getItem("scimux-people-draft:n1"), null);
  assert.equal(storage.getItem("scimux-retired-index:n1"), null);
  assert.equal(storage.getItem("scimux-retired-drafts:n1:old"), null);
  assert.equal(storage.getItem("scimux-retired-flight:n1\0old"), "leave this");
  assert.equal(storage.getItem("scimux-retired-visible:n1\0old"), "leave visible");
});

test("bindDraftIdentity removes a damaged archive without throwing", () => {
  const damaged = memory({
    "scimux-draft-owner:n1": "current",
    "scimux-draft:n1": "live",
    "scimux-retired-index:n1": "not json",
    "scimux-retired-drafts:n1:bad": "not json either",
  });
  assert.equal(links.bindDraftIdentity(damaged, "n1", "current"), true);
  assert.equal(damaged.getItem("scimux-draft:n1"), "live");
  assert.equal(damaged.getItem("scimux-retired-index:n1"), null);
  assert.equal(damaged.getItem("scimux-retired-drafts:n1:bad"), null);

  const entry = memory({
    "scimux-draft-owner:n1": "current",
    "scimux-retired-index:n1": JSON.stringify(["bad"]),
    "scimux-retired-drafts:n1:bad": "{",
  });
  assert.equal(links.bindDraftIdentity(entry, "n1", "current"), true);
  assert.equal(entry.getItem("scimux-retired-index:n1"), null);
  assert.equal(entry.getItem("scimux-retired-drafts:n1:bad"), null);

  const notArray = memory({
    "scimux-draft-owner:n1": "current",
    "scimux-retired-index:n1": "{}",
    "scimux-retired-drafts:n1:unknown": JSON.stringify([{"scimux-draft:": "nope"}]),
  });
  assert.equal(links.bindDraftIdentity(notArray, "n1", "current"), true);
  assert.equal(notArray.getItem("scimux-draft:n1"), null);
  assert.equal(notArray.getItem("scimux-retired-index:n1"), null);
  assert.equal(notArray.getItem("scimux-retired-drafts:n1:unknown"), null);

  const thrown = memory({
    "scimux-draft-owner:n1": "current",
    "scimux-retired-index:n1": "not json",
    "scimux-retired-drafts:n1:bad": "kept when keys fail",
  });
  thrown.keys = () => { throw new Error("no keys"); };
  assert.equal(links.bindDraftIdentity(thrown, "n1", "current"), true);
  assert.equal(thrown.getItem("scimux-retired-index:n1"), null);

  const noList = memory({
    "scimux-draft-owner:n1": "current",
    "scimux-retired-index:n1": "not json",
    "scimux-retired-drafts:n1:bad": "unlisted",
  });
  noList.keys = () => null;
  assert.equal(links.bindDraftIdentity(noList, "n1", "current"), true);
  assert.equal(noList.getItem("scimux-retired-index:n1"), null);
  assert.equal(noList.getItem("scimux-retired-drafts:n1:bad"), "unlisted");

  const mixed = memory({
    "scimux-draft-owner:n1": "current",
    "scimux-retired-index:n1": "not json",
    "scimux-retired-drafts:n1:bad": "gone",
    "other": "stay",
  });
  mixed.keys = () => [1, "scimux-retired-drafts:n1:bad", "other"];
  assert.equal(links.bindDraftIdentity(mixed, "n1", "current"), true);
  assert.equal(mixed.getItem("scimux-retired-drafts:n1:bad"), null);
  assert.equal(mixed.getItem("other"), "stay");
  assert.equal(mixed.getItem("scimux-retired-index:n1"), null);

  const owners = memory({
    "scimux-draft-owner:n1": "current",
    "scimux-retired-index:n1": JSON.stringify(["old", 1, null]),
    "scimux-retired-drafts:n1:old": JSON.stringify([{"scimux-draft:": "do not restore"}]),
  });
  assert.equal(links.bindDraftIdentity(owners, "n1", "current"), true);
  assert.equal(owners.getItem("scimux-draft:n1"), null);
  assert.equal(owners.getItem("scimux-retired-drafts:n1:old"), null);
  assert.equal(owners.getItem("scimux-retired-index:n1"), null);

  for (const raw of ["{}", "[]", "[null]", '["x"]', '[{"scimux-draft:":"earlier"},[]]', "not json"]) {
    const storage = memory({
      "scimux-draft-owner:n1": "current",
      "scimux-retired-index:n1": JSON.stringify(["unknown"]),
      "scimux-retired-drafts:n1:unknown": raw,
    });
    assert.equal(links.bindDraftIdentity(storage, "n1", "current"), true, raw);
    assert.equal(storage.getItem("scimux-draft:n1"), null, raw);
    assert.equal(storage.getItem("scimux-retired-index:n1"), null, raw);
    assert.equal(storage.getItem("scimux-retired-drafts:n1:unknown"), null, raw);
  }

  const withoutKeys = {
    getItem(key){ return key === "scimux-retired-index:n1" ? "not json" : key === "scimux-draft-owner:n1" ? "current" : null; },
    setItem(){},
    removeItem(){},
  };
  assert.equal(links.bindDraftIdentity(withoutKeys, "n1", "current"), true);

  const absent = memory({"scimux-draft-owner:n1": "current", "scimux-draft:n1": "live"});
  assert.equal(links.bindDraftIdentity(absent, "n1", "current"), true);
  assert.equal(absent.getItem("scimux-draft:n1"), "live");
});

test("bindDraftIdentity fails closed when storage throws", () => {
  assert.equal(links.bindDraftIdentity({getItem(){ throw new Error("unavailable"); }, setItem(){}, removeItem(){}}, "n1", "new"), false);
  assert.equal(links.bindDraftIdentity({getItem(){ return null; }, setItem(){ throw new Error("full"); }, removeItem(){}}, "n1", "new"), false);
  assert.equal(links.bindDraftIdentity({getItem(){ throw new Error("unavailable"); }}, "n1", ""), true);
});

test('asynchronous agent rejection restores only matching in-flight prompt sources', () => {
  const storage=memory(); const first={uid:'u',segment:0,record:1}, other={uid:'u',segment:0,record:2};
  links.writePromptFlights(storage,'n1',[{id:1,text:'rejected',sources:[first]},{id:2,text:'later',sources:[other]}]);
  links.recoverPromptFlights(storage,'n1','rejected');
  assert.deepEqual(links.readPromptSources(storage,'n1'),[first]);
  assert.deepEqual(links.readPromptFlights(storage,'n1'),[{id:2,text:'later',sources:[other]}]);
  links.recoverPromptFlights(storage,'n1','unrelated');
  assert.equal(links.readPromptFlights(storage,'n1').length,1);
});

test('prompt source and flight stores reject malformed caches and tolerate storage refusal', () => {
  const s=memory({'scimux-prompt-sources:n':'{}','scimux-prompt-flights:n':'{}'});
  assert.deepEqual(links.readPromptSources(s,'n'),[]); assert.deepEqual(links.readPromptFlights(s,'n'),[]);
  const broken={getItem(){throw Error('offline')},setItem(){throw Error('offline')},removeItem(){throw Error('offline')}};
  assert.deepEqual(links.readPromptSources(broken,'n'),[]); assert.deepEqual(links.readPromptFlights(broken,'n'),[]);
  assert.doesNotThrow(()=>links.writePromptFlights(broken,'n',[{sources:[]}]));
  assert.doesNotThrow(()=>links.writePromptSources(broken,'n',[]));
});

test('rejection recovery only considers one accepted flight and a matching newer user row', () => {
 const f={accepted:true,text:'draft',boundary:{uid:'u',segment:0,record:1},sources:[{uid:'u',segment:0,record:1}]};
 const rows=[{uid:'u',segment:0,record:1,role:'assistant'},{uid:'u',segment:0,record:2,role:'user',text:'draft'}];
 assert.equal(links.rejectedPromptFlight([f],rows),f);
 assert.equal(links.rejectedPromptFlight([f,f],rows),null);
 assert.equal(links.rejectedPromptFlight([{...f,accepted:false}],rows),null);
 assert.equal(links.rejectedPromptFlight([f],rows.slice(1)),null);
 assert.equal(links.rejectedPromptFlight([f],[rows[0],{...rows[1],text:'other'}]),null);
});

test('omitted zero segment in server JSON still identifies the exact first segment', () => {
 const source={uid:'u',record:1};
 assert.equal(links.promptSourceKey(source),links.promptSourceKey({...source,segment:0}));
 const flight={accepted:true,text:'draft',boundary:source,sources:[source]};
 assert.equal(links.rejectedPromptFlight([flight],[{...source,role:'assistant'},{uid:'u',record:2,role:'user',text:'draft'}]),flight);
});

test('rejection matching tolerates missing rows and missing prompt text', () => {
 const f={accepted:true,text:'draft',boundary:{uid:'u',record:1},sources:[{uid:'u',record:1}]};
 assert.equal(links.rejectedPromptFlight([f],null),null);
 assert.equal(links.rejectedPromptFlight([f],[{uid:'u',record:1},{uid:'u',record:2,role:'user'}]),null);
});

test('prompt link stores full shared provenance and exact zero-segment destination', () => {
 const source={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:1,role:'person',who:'Alex',prov:true};
 const link=links.makePromptLink(source,'n1',{uid:'u',record:4});
 assert.equal(link.kind,'prompt');assert.deepEqual(link.source,source);assert.deepEqual(link.destination,{node:'n1',uid:'u',segment:0,record:4});
});

test('forward cache keys and legacy addresses tolerate missing identifiers', () => {
  assert.equal(links.pendingForwardKey(), 'scimux-sendto-pending:');
  assert.equal(links.awaitingForwardKey(), 'scimux-sendto-awaiting:');
  assert.equal(links.sourceAddressKey({}), 'n::');
});

test('confirmed prompt display preview is separate from its exact address', () => {
 const source={uid:'u',segment:0,record:1},link=links.makePromptLink(source,'n1',{uid:'u',segment:0,record:4},'Edited   prompt\nline');
 assert.equal(link.preview,'Edited prompt line');assert.deepEqual(link.destination,{node:'n1',uid:'u',segment:0,record:4});
 assert.equal('text' in link.destination,false);assert.equal('turnTime' in link.destination,false);
});

test('prompt preview is bounded and optional for older links', () => {
 const source={uid:'u',record:1},address={uid:'u',record:2};
 assert.equal(links.makePromptLink(source,'n',address).preview,undefined);
 assert.equal(Array.from(links.makePromptLink(source,'n',address,'🙂'.repeat(110)).preview).length,100);
});


