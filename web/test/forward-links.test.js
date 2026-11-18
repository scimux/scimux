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

test('draft identity retains retired and unattributed data without merging it into a new chat', () => {
 const storage=memory({'scimux-draft-owner:n1':'old','scimux-draft:n1':'old text','scimux-people-draft:n1':'old discussion','scimux-prompt-sources:n1':'[{"uid":"old"}]'});
 assert.equal(links.bindDraftIdentity(storage,'n1','new'),true);
 assert.equal(storage.getItem('scimux-draft:n1'),null);assert.equal(storage.getItem('scimux-people-draft:n1'),null);
 const archived=JSON.parse(storage.getItem('scimux-retired-drafts:n1:old'));assert.equal(archived[0]['scimux-draft:'],'old text');assert.equal(archived[0]['scimux-people-draft:'],'old discussion');
 storage.setItem('scimux-draft:n1','new text');assert.equal(links.bindDraftIdentity(storage,'n1','new'),true);assert.equal(storage.getItem('scimux-draft:n1'),'new text');
 const legacy=memory({'scimux-draft:n1':'unattributed'});assert.equal(links.bindDraftIdentity(legacy,'n1','fresh'),true);assert.equal(JSON.parse(legacy.getItem('scimux-retired-drafts:n1:unknown'))[0]['scimux-draft:'],'unattributed');
 assert.equal(links.bindDraftIdentity(legacy,'n1',''),true);
});

test('draft identity fails closed when it cannot archive or read prior data', () => {
 const storage=memory({'scimux-draft-owner:n1':'old','scimux-draft:n1':'keep','scimux-retired-drafts:n1:old':'{}'});
 assert.equal(links.bindDraftIdentity(storage,'n1','new'),false);assert.equal(storage.getItem('scimux-draft:n1'),'keep');
 assert.equal(links.bindDraftIdentity({getItem(){throw Error('unavailable')}},'n1','new'),false);
});

test('unattributed and retired drafts can be listed for explicit local review', () => {
 const storage=memory({'scimux-draft:n1':'legacy text','scimux-prompt-sources:n1':'[{"uid":"u","record":1}]'});
 assert.equal(links.bindDraftIdentity(storage,'n1','new'),true);
 const entries=links.readRetiredDrafts(storage,'n1');
 assert.equal(entries.length,1);assert.equal(entries[0].owner,'unknown');
 assert.equal(entries[0].values['scimux-draft:'],'legacy text');
 assert.equal(JSON.parse(entries[0].values['scimux-prompt-sources:'])[0].uid,'u');
 assert.equal(links.archiveRetiredDraft(storage,'n1','old',{'scimux-draft:':'interrupted send','scimux-prompt-sources:':'[{"uid":"v","record":2}]'}),true);
 const added=links.readRetiredDrafts(storage,'n1').find(entry=>entry.owner==='old');
 assert.equal(added.values['scimux-draft:'],'interrupted send');
 assert.equal(JSON.parse(added.values['scimux-prompt-sources:'])[0].uid,'v');
});

test('damaged recovery entries are ignored without hiding intact saved drafts', () => {
 const storage=memory({'scimux-retired-index:n1':'["bad","wrong","missing","good"]','scimux-retired-drafts:n1:bad':'not json','scimux-retired-drafts:n1:wrong':'{}','scimux-retired-drafts:n1:good':'[null,[],{"scimux-draft:":"safe"}]'});
 assert.deepEqual(links.readRetiredDrafts(storage,'n1'),[{owner:'good',values:{'scimux-draft:':'safe'}}]);
 storage.setItem('scimux-retired-index:n1','{}');assert.deepEqual(links.readRetiredDrafts(storage,'n1'),[]);
 assert.deepEqual(links.readRetiredDrafts({getItem(){throw Error('offline')}},'n1'),[]);
 assert.equal(links.archiveRetiredDraft(storage,'n1','good',{}),false);
 assert.equal(links.archiveRetiredDraft({getItem(){throw Error('offline')}},'n1','old',{}),false);
 storage.setItem('scimux-retired-index:n1','["good"]');
 assert.equal(links.archiveRetiredDraft(storage,'n1','good',{'scimux-draft:':'another'}),true);
 assert.deepEqual(links.readRetiredDrafts(storage,'n1').map(entry=>entry.values['scimux-draft:']),['safe','another']);
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
