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
