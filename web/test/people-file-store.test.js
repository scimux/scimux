import test from "node:test";
import assert from "node:assert/strict";
import {createPeopleFileStore,createIndexedDBPeopleDriver,createBrowserPeopleFileStore} from "../js/people-file-store.js";

const recoveryPeopleKey = ['a'.repeat(32),'b'.repeat(32),'c'.repeat(16),'d'.repeat(16)].join('/');
function recoveryExpected(db, publication) { const row=db.recovery.get('publication:'+publication); return {key:row.peopleKey,copyInstance:row.copyInstance,binding:row.binding,instance:row.native,head:row.head,version:row.version,publicationKey:row.request.key,text:row.request.text,attachments:row.request.attachments,value:JSON.stringify({instance:row.native,copyInstance:row.copyInstance,request:row.request})}; }

function fakeDriver() {
 let rows=[];
 return {async readWrite(fn){const result=fn(rows.map(row=>({...row})));rows=result.rows;return result.value;},async read(){return rows.map(row=>({...row}));}};
}

test('file identities use cryptographic random values without randomUUID',async()=>{
 const previous=Object.getOwnPropertyDescriptor(globalThis,'crypto');
 const driver=fakeDriver(),store=createPeopleFileStore(driver),file={name:'example.txt',type:'text/plain',size:1};let fill=1;
 try {
  Object.defineProperty(globalThis,'crypto',{configurable:true,value:{getRandomValues:bytes=>bytes.fill(fill++)}});
  const first=await store.save('scope','0123456789abcdef',file);
  const second=await store.save('scope','fedcba9876543210',file);
  assert.notEqual(first,second);assert.equal((await store.list('scope')).length,2);
  Object.defineProperty(globalThis,'crypto',{configurable:true,value:{getRandomValues(){throw Error('unavailable')}}});
  await assert.rejects(store.save('scope','1111111111111111',file),/unavailable/i);
  assert.equal((await store.list('scope')).length,2);
  Object.defineProperty(globalThis,'crypto',{configurable:true,value:{}});
  await assert.rejects(store.save('scope','1111111111111111',file),/unavailable/i);
  assert.equal((await store.list('scope')).length,2);
 } finally { if(previous) Object.defineProperty(globalThis,'crypto',previous); else delete globalThis.crypto; }
});

test('stage revision stays with the original scoped file across reload', async()=>{
 const driver=fakeDriver(), store=createPeopleFileStore(driver), file={name:'synthetic.txt',type:'text/plain',size:1};
 const id='0123456789abcdef', revision='a'.repeat(64);
 const token=await store.save('copy-a',id,file); await store.save('copy-b',id,file);
 await store.setStageRevision('copy-a',id,revision,token);
 assert.equal((await createPeopleFileStore(driver).list('copy-a'))[0].stageRevision,revision);
 assert.equal((await store.list('copy-b'))[0].stageRevision,undefined);
 await assert.rejects(store.setStageRevision('copy-a',id,'b'.repeat(64),token),/conflict/i);
 assert.equal((await store.list('copy-a'))[0].stageRevision,revision);
 await store.setStageRevision('copy-a',id,revision,token);
 for (const args of [['',id,revision,token],['copy-a','bad',revision,token],['copy-a',id,'bad',token],['copy-a',id,revision,'bad']]) await assert.rejects(store.setStageRevision(...args),/identity/i);
 await assert.rejects(store.setStageRevision('copy-a','fedcba9876543210',revision,token),/recovery/i);
});

test('a late stage result cannot modify or remove a replacement browser file',async()=>{
 const store=createPeopleFileStore(fakeDriver()),id='0123456789abcdef',file={name:'old.txt',type:'text/plain',size:1};
 const original=await store.save('scope',id,file);await store.remove('scope',id);
 const replacement=await store.save('scope',id,{...file,name:'new.txt'});
 await assert.rejects(store.setStageRevision('scope',id,'a'.repeat(64),original),/conflict/i);
 await assert.rejects(store.removeStageFile('scope',id,original),/conflict/i);
 await assert.rejects(store.markPublishedBatch('scope',[id],1,[original]),/conflict/i);
 const rows=await store.list('scope');assert.equal(rows[0].file.name,'new.txt');assert.equal(rows[0].stageRevision,undefined);assert.notEqual(original,replacement);
 await store.removeStageFile('scope',id,replacement);assert.deepEqual(await store.list('scope'),[]);
 for(const args of [['',id,replacement],['scope','bad',replacement],['scope',id,'bad']]) await assert.rejects(store.removeStageFile(...args),/identity/i);
 await store.removeStageFile('scope',id,undefined);
 for(const tokens of [null,[]]) await assert.rejects(store.markPublishedBatch('scope',[id],1,tokens),/identity/i);
});

test('legacy rows retain their identity when reviewed',async()=>{
 const file={name:'legacy.txt',type:'text/plain',size:1},row={key:'scope\0'+'0123456789abcdef',scope:'scope',id:'0123456789abcdef',file};
 const driver={read:async()=>[row],readWrite:async fn=>fn([row]).value},store=createPeopleFileStore(driver);
 assert.equal((await store.list('scope'))[0].fileToken,undefined);
 await assert.rejects(store.markPublishedBatch('scope',[row.id],1,[undefined]),/review/i);
 await store.removeStageFile('scope',row.id,undefined);
});

test("People file store keeps raw files isolated and rejects identity reuse", async()=>{
 const store=createPeopleFileStore(fakeDriver());const file={name:"synthetic.txt",type:"text/plain",size:12};
 await store.save("s/o/c/g\0owner", "0123456789abcdef", file);
 assert.equal((await store.list("s/o/c/g\0owner"))[0].file,file);
 assert.deepEqual(await store.list("s/o/c/other\0owner"),[]);
 await assert.rejects(store.save("s/o/c/g\0owner","0123456789abcdef",{...file,name:"different.txt"}),/conflict/i);
 await store.remove("s/o/c/g\0owner","0123456789abcdef");assert.deepEqual(await store.list("s/o/c/g\0owner"),[]);
});

test('retired People files are scoped by channel and native owner identity',async()=>{
 const store=createPeopleFileStore(fakeDriver()),file={name:'synthetic.txt',type:'text/plain',size:1},prefix='people\0s/o/c/g\0binding:server',owner='n1\0native',old=prefix+'\0'+owner,prior=prefix+'\0copy:'+'a'.repeat(32)+'\0'+owner,current=prefix+'\0copy:'+'b'.repeat(32)+'\0'+owner;
 await store.save(old,'0123456789abcdef',file);await store.save(prior,'0123456789abcdef',file);await store.save(current,'0123456789abcdef',file);await store.save(prefix+'\0copy:'+'c'.repeat(32)+'\0n1\0other','0123456789abcdef',file);
 const rows=await store.listRetired(prefix,owner,current);assert.deepEqual(rows.map(row=>row.scope).sort(),[old,prior].sort());assert.equal(rows.length,2);
});

test('retired file review rejects invalid scope and retains publication status',async()=>{
 const store=createPeopleFileStore(fakeDriver()),file={name:'synthetic.txt',type:'text/plain',size:1},prefix='people\0s/o/c/g',owner='n1\0native',old=prefix+'\0'+owner,current=prefix+'\0copy:'+'b'.repeat(32)+'\0'+owner;
 for(const args of [[null,owner,current],[prefix,null,current],[prefix,owner,'']])await assert.rejects(store.listRetired(...args),/identity/i);
 await store.save(old,'0123456789abcdef',file);await store.markPublished(old,'0123456789abcdef',7);await store.save('unrelated','fedcba9876543210',file);
 const rows=await store.listRetired(prefix,owner,current);assert.equal(rows.length,1);assert.equal(rows[0].published,7);
});

test("People file store refuses empty and oversized files without losing prior drafts", async()=>{
 const store=createPeopleFileStore(fakeDriver());await store.save("scope","0123456789abcdef",{name:"ok.txt",type:"text/plain",size:1});
 await assert.rejects(store.save("scope","fedcba9876543210",{name:"empty.txt",type:"text/plain",size:0}),/empty|size/i);
 await assert.rejects(store.save("scope","1111111111111111",{name:"large.txt",type:"text/plain",size:4194305}),/size|4 MiB/i);
 assert.equal((await store.list("scope")).length,1);
});

function fakeIndexedDB({legacyRows=[]}={}) {
 const records=new Map(legacyRows.map(row=>[row.key,row])), names=new Set(legacyRows.length?["files"]:[]), recovery=new Map();
 const db={objectStoreNames:{contains:name=>names.has(name)},createObjectStore(name){names.add(name);return {add(row){recovery.set(row.key,row);}};},close(){},transaction(_name,mode){
  const working=new Map(records), recoveryWorking=new Map(recovery);let pending=0;let aborted=false;let scheduled=false;
  const tx={error:null,abort(){aborted=true;queueMicrotask(()=>tx.onabort?.());},objectStore(name){const selected=name==="recovery"?recoveryWorking:working;return {
   getAll(){return request(()=>[...selected.values()]);},
   put(row){return request(()=>{selected.set(row.key,row);return row.key;});},
   add(row){return request(()=>{if(selected.has(row.key))throw Error("collision");selected.set(row.key,row);return row.key;});},
   delete(key){return request(()=>{selected.delete(key);});},
  }}};
  function finish(){if(scheduled)return;scheduled=true;queueMicrotask(()=>{scheduled=false;if(pending || aborted)return;if(mode==="readwrite"){records.clear();for(const [k,v] of working)records.set(k,v);recovery.clear();for(const [k,v] of recoveryWorking)recovery.set(k,v)}tx.oncomplete?.();});}
  function request(fn){const req={};pending++;queueMicrotask(()=>{if(aborted)return;try{req.result=fn();req.onsuccess?.()}catch(err){req.error=err;tx.error=err;req.onerror?.();tx.abort()}finally{pending--;finish()}});return req;}
  return tx;
 }};
 return {db,recovery,open(){const req={};queueMicrotask(()=>{req.result=db;if(!names.has("recovery"))req.onupgradeneeded?.();req.onsuccess?.()});return req;}};
}

test("browser People storage is optional but uses IndexedDB when available", async()=>{
 assert.equal(createBrowserPeopleFileStore(null),null);
 const store=createBrowserPeopleFileStore(fakeIndexedDB());
 await store.save("scope","0123456789abcdef",{name:"x",type:"text/plain",size:1});
 assert.equal((await store.list("scope")).length,1);
});

test('captured People draft discard rejects a changed reviewed draft value',async()=>{
 const db=fakeIndexedDB(),store=createBrowserPeopleFileStore(db),scope='people\0synthetic\0',original=await store.savePeopleDraft({scope,text:'captured text'}),captured=await store.freezePeopleDraft({scope,text:'captured text',...original});
 assert.ok(db.recovery.get('head:'+captured.head).sealed);assert.equal(db.recovery.get('draft:'+captured.version).text,'captured text');const before=JSON.stringify([...db.recovery.values()]);
 const result=await store.discardPeopleCapture({scope,...captured,text:'different reviewed text',current:()=>true});assert.equal(result,false,'discard must compare the exact reviewed draft value');assert.equal(JSON.stringify([...db.recovery.values()]),before);
});

test('captured People draft discard uses exact identity and preserves unrelated data',async()=>{
 const db=fakeIndexedDB(),store=createBrowserPeopleFileStore(db),scope='people\0synthetic\0',original=await store.savePeopleDraft({scope,text:'kept input'}),captured=await store.freezePeopleDraft({scope,text:'kept input',...original}),file={name:'kept.txt',type:'text/plain',size:2};await store.save(scope,'0123456789abcdef',file);const input={scope,...captured,text:'kept input',current:()=>true},before=JSON.stringify([...db.recovery.values()]),next=db.recovery.get('allocator').next;
 for(const bad of [{...input,scope:'other'}, {...input,head:'wrong'}, {...input,version:'wrong'}, {...input,head:original.head,version:original.version}, {...input,current:()=>false}]){assert.equal(await store.discardPeopleCapture(bad),false);assert.equal(JSON.stringify([...db.recovery.values()]),before);}
 for(const bad of [{...input,scope:''},{...input,head:null},{...input,version:null},{...input,text:null},{...input,current:null}])await assert.rejects(store.discardPeopleCapture(bad),/identity/i);
 assert.equal(await store.clearPeopleDraft({scope,...captured}),false);assert.equal(await store.discardPeopleCapture(input),true);assert.equal(db.recovery.has('head:'+captured.head),false);assert.equal(db.recovery.has('draft:'+captured.version),false);assert.ok(db.recovery.has('head:'+original.head));assert.equal(db.recovery.get('allocator').next,next);assert.equal((await store.list(scope))[0].file.name,'kept.txt');assert.equal(await store.discardPeopleCapture(input),false);
});

test('captured People draft discard refuses live and terminal publication references',async()=>{
 for(const kind of ['text-pending','file-pending','retired','completed','other-head','source-head']){
  const db=fakeIndexedDB(),store=createBrowserPeopleFileStore(db),scope='people\0synthetic\0',original=await store.savePeopleDraft({scope,text:'saved'}),captured=await store.freezePeopleDraft({scope,text:'saved',...original}),input={scope,...captured,text:'saved',current:()=>true},copyInstance='1'.repeat(32),request={key:'2'.repeat(16),text:'saved',anchor:0};
  if(kind==='text-pending')await store.beginPeopleTextPublication({scope,peopleKey:recoveryPeopleKey,copyInstance,binding:'',native:'',...captured,request});
  if(kind==='file-pending'||kind==='retired'||kind==='completed'){const id='3'.repeat(16),token=await store.save(scope,id,{name:'kept.txt',type:'text/plain',size:1}),revision='a'.repeat(64);await store.setStageRevision(scope,id,revision,token);const saved=await store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,copyInstance,binding:'',native:'',...captured,request:{...request,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]});if(kind!=='file-pending'){const expected=recoveryExpected(db,saved.publication);await store.decidePeoplePublication({scope,publication:saved.publication,expected,proof:kind==='retired'?{state:'locally_discarded'}:{state:'completed',seq:7,text:'saved',stage_ids:[id]}});}}
  if(kind==='retired')db.recovery.set('head:retained-reference',{key:'head:retained-reference',kind:'head',scope,head:'retained-reference',version:captured.version});
  if(kind==='other-head')db.recovery.set('head:other',{key:'head:other',kind:'head',scope,head:'other',version:captured.version});
  if(kind==='source-head')await store.freezePeopleDraft({scope,text:'saved',...captured});
  const before=JSON.stringify([...db.recovery.values()]);assert.equal(await store.discardPeopleCapture(input),false,kind);assert.equal(JSON.stringify([...db.recovery.values()]),before,kind);
 }
});

test('an exact locally retired capture can discard only its redundant head and draft',async()=>{
 const db=fakeIndexedDB(),store=createBrowserPeopleFileStore(db),scope='people\0synthetic\0',copyInstance='1'.repeat(32),id='3'.repeat(16),revision='a'.repeat(64);
 const file={name:'kept.txt',type:'text/plain',size:1},token=await store.save(scope,id,file);await store.setStageRevision(scope,id,revision,token);
 const original=await store.savePeopleDraft({scope,text:'saved'}),captured=await store.freezePeopleDraft({scope,text:'saved',...original});
 const saved=await store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,copyInstance,binding:'',native:'',...captured,request:{key:'2'.repeat(16),text:'saved',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]});
 await store.decidePeoplePublication({scope,publication:saved.publication,expected:recoveryExpected(db,saved.publication),proof:{state:'locally_discarded'}});
 const before=[...db.recovery.values()].map(row=>JSON.parse(JSON.stringify(row))),filesBefore=await store.list(scope);
 assert.equal(before.find(row=>row.key==='decision:'+saved.publication).state,'retired');
 assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),true);
 assert.deepEqual([...db.recovery.values()],before.filter(row=>row.key!=='head:'+captured.head&&row.key!=='draft:'+captured.version));
 assert.deepEqual(await store.list(scope),filesBefore);
});

test('captured People discard and text admission serialize in either order',async()=>{
 for(const first of ['discard','admit']){const db=fakeIndexedDB(),store=createBrowserPeopleFileStore(db),scope='people\0synthetic\0',draft=await store.savePeopleDraft({scope,text:'saved'}),captured=await store.freezePeopleDraft({scope,text:'saved',...draft}),discard={scope,...captured,text:'saved',current:()=>true},admit={scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'',native:'',...captured,request:{key:'2'.repeat(16),text:'saved',anchor:0},current:()=>true};
  if(first==='discard'){assert.equal(await store.discardPeopleCapture(discard),true);await assert.rejects(store.beginPeopleTextPublication(admit),/changed/i);assert.equal([...db.recovery.values()].filter(row=>row.kind==='text-publication').length,0);}else{const created=await store.beginPeopleTextPublication(admit);assert.equal(await store.discardPeopleCapture(discard),false);assert.ok(db.recovery.has('text-publication:'+created.publication));assert.ok(db.recovery.has('head:'+captured.head));}
 }
});

test("IndexedDB driver commits drafts atomically and recovers across store instances", async()=>{
 const indexedDB=fakeIndexedDB();const first=createPeopleFileStore(createIndexedDBPeopleDriver(indexedDB));const file={name:"synthetic.txt",type:"text/plain",size:12};
 await first.save("scope-a","0123456789abcdef",file);const restarted=createPeopleFileStore(createIndexedDBPeopleDriver(indexedDB));
 assert.equal((await restarted.list("scope-a"))[0].file.name,"synthetic.txt");assert.deepEqual(await restarted.list("scope-b"),[]);
 await assert.rejects(restarted.save("scope-a","0123456789abcdef",file),/conflict/i);assert.equal((await restarted.list("scope-a")).length,1);
 await restarted.remove("scope-a","0123456789abcdef");assert.deepEqual(await first.list("scope-a"),[]);
});

test("IndexedDB retries after a synchronous open failure", async()=>{
 const working=fakeIndexedDB();let calls=0;
 const driver=createIndexedDBPeopleDriver({open(...args){if (++calls===1) throw Error("unavailable");return working.open(...args);}});
 await assert.rejects(driver.read(),/unavailable/i);
 assert.deepEqual(await driver.read(),[]);assert.equal(calls,2);
});

test("People file store enforces per-chat and global limits without deleting saved files", async()=>{
 const store=createPeopleFileStore(fakeDriver());for(let i=0;i<16;i++)await store.save("scope",i.toString(16).padStart(16,"0"),{name:"synthetic.txt",type:"text/plain",size:1});
 await assert.rejects(store.save("scope","ffffffffffffffff",{name:"synthetic.txt",type:"text/plain",size:1}),/limit/i);assert.equal((await store.list("scope")).length,16);
 const totals=createPeopleFileStore(fakeDriver());for(let i=0;i<4;i++)await totals.save("scope",i.toString(16).padStart(16,"0"),{name:"synthetic.txt",type:"text/plain",size:4194304});
 await assert.rejects(totals.save("scope","ffffffffffffffff",{name:"synthetic.txt",type:"text/plain",size:1}),/limit/i);assert.equal((await totals.list("scope")).length,4);
});

test("People file store fails closed on invalid identity, unsafe name and damaged records", async()=>{
 const store=createPeopleFileStore(fakeDriver());await assert.rejects(store.save("","0123456789abcdef",{name:"x",type:"text/plain",size:1}),/identity/i);
 await assert.rejects(store.save("scope","wrong",{name:"x",type:"text/plain",size:1}),/identity/i);
 await assert.rejects(store.save("scope","0123456789abcdef",{name:"../x",type:"text/plain",size:1}),/safe name/i);
 await assert.rejects(store.remove("scope","wrong"),/identity/i);await assert.rejects(store.list(""),/identity/i);
 const damaged=createPeopleFileStore({read:async()=>[{scope:"scope",id:"wrong",file:{}}],readWrite:async fn=>fn([{scope:"scope",id:"wrong",file:{}}])});
 await assert.rejects(damaged.list("scope"),/recovery/i);await assert.rejects(damaged.remove("scope","0123456789abcdef"),/recovery/i);
 assert.throws(()=>createIndexedDBPeopleDriver(null),/unavailable/i);
});

test("People file store records confirmed delivery before cleanup and cannot change its sequence", async()=>{
 const driver=fakeDriver(),store=createPeopleFileStore(driver);await store.save("scope","0123456789abcdef",{name:"synthetic.txt",type:"text/plain",size:2});
 await store.markPublished("scope","0123456789abcdef",7);const reloaded=createPeopleFileStore(driver);const rows=await reloaded.list("scope");assert.equal(rows[0].published,7);
 await assert.rejects(reloaded.markPublished("scope","0123456789abcdef",8),/conflict/i);assert.equal((await reloaded.list("scope"))[0].published,7);
});

test("People delivery markers are atomic across the frozen file list", async()=>{
 const store=createPeopleFileStore(fakeDriver());const file={name:"x",type:"text/plain",size:1};
 await store.save("scope","0123456789abcdef",file);await store.save("scope","fedcba9876543210",file);
 await assert.rejects(store.markPublishedBatch("scope",["0123456789abcdef","1111111111111111"],7),/recovery/i);
 assert.deepEqual((await store.list("scope")).map(row=>row.published),[undefined,undefined]);
 await store.markPublishedBatch("scope",["0123456789abcdef","fedcba9876543210"],7);
 assert.deepEqual((await store.list("scope")).map(row=>row.published),[7,7]);
});

test("prepared source file keeps its logical association across reload",async()=>{
 const driver=fakeDriver(),store=createPeopleFileStore(driver);const source={server:"a".repeat(32),owner:"b".repeat(32),chat:"c".repeat(16),generation:"d".repeat(16),log:"comments",seq:1};
 await store.save("agent-source\0node\0instance","0123456789abcdef",{name:"source.txt",type:"text/plain",size:1},{source,sourceFileID:"fedcba9876543210"});
 const rows=await createPeopleFileStore(driver).list("agent-source\0node\0instance");assert.deepEqual(rows[0].metadata,{source,sourceFileID:"fedcba9876543210"});
});

test('source file metadata must be a bounded valid full association',async()=>{
 assert.throws(()=>createPeopleFileStore(null),/unavailable/);
 const store=createPeopleFileStore(fakeDriver()),file={name:'x',type:'text/plain',size:1},source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1},metadata={source,sourceFileID:'fedcba9876543210'};
 const cyclic={...metadata};cyclic.extra=cyclic;
 for(const invalid of [null,{}, {...metadata,sourceFileID:'bad'}, {...metadata,source:null}, ...['server','owner','chat','generation'].map(k=>({...metadata,source:{...source,[k]:undefined}})), {...metadata,source:{...source,log:'mirror'}}, {...metadata,source:{...source,seq:0}}, {...metadata,extra:'x'.repeat(4097)},cyclic]) await assert.rejects(store.save('scope','0123456789abcdef',file,invalid),/identity/);
 for(const bad of [[],['bad'],['0123456789abcdef','0123456789abcdef']]) await assert.rejects(store.markPublishedBatch('scope',bad,1),/identity/);
});

test('IndexedDB handles blocked upgrades, late successes and version changes',async()=>{
 let closed=0;const broken={open(){const request={};queueMicrotask(()=>{request.result={objectStoreNames:{contains:()=>false},createObjectStore(){throw Error('broken')},close(){closed++}};request.onupgradeneeded();request.onsuccess()});return request}};
 await assert.rejects(createIndexedDBPeopleDriver(broken).read(),/unavailable/);assert.equal(closed,1);
 const working=fakeIndexedDB(),driver=createIndexedDBPeopleDriver(working);await driver.read();working.db.onversionchange();await driver.read();
});

test('IndexedDB reports failed transaction construction, requests and aborts',async()=>{
 for(const kind of ['construct','get','request','abort']) {
  const db={transaction(){if(kind==='construct')throw Error('synthetic');const tx={abort(){queueMicrotask(()=>tx.onabort())},objectStore(){if(kind==='get')throw Error('synthetic');return {getAll(){const request={};queueMicrotask(()=>{if(kind==='request')request.onerror();else tx.onabort()});return request}}}};return tx}};
  const indexedDB={open(){const request={};queueMicrotask(()=>{request.result=db;request.onsuccess()});return request}};
  await assert.rejects(createIndexedDBPeopleDriver(indexedDB).read(),/unavailable/);
 }
});

for (const action of ['mark','remove']) test(`queued People ${action} checks ownership when its serialized update runs`, async () => {
  let rows=[], held=null, pause=false, current=true;
  const driver={read:async()=>rows.map(row=>({...row})),readWrite:fn=>pause?new Promise((resolve,reject)=>{held=()=>{try{const result=fn(rows.map(row=>({...row})));rows=result.rows;resolve(result.value);}catch(err){reject(err);}};}):Promise.resolve().then(()=>{const result=fn(rows.map(row=>({...row})));rows=result.rows;return result.value;})};
  const store=createPeopleFileStore(driver), id='0123456789abcdef', other='fedcba9876543210', file={name:'saved.txt',type:'text/plain',size:3}, token=await store.save('scope',id,file);
  await store.save('scope',other,{name:'other.txt',type:'text/plain',size:2});pause=true;
  const operation=action==='mark'?store.markPublishedBatch('scope',[id],7,[token],()=>current):store.removeStageFile('scope',id,token,()=>current);
  const rejected=assert.rejects(operation,/cancel/i);assert.equal(typeof held,'function');current=false;held();await rejected;
  const saved=await store.list('scope');assert.equal(saved.length,2);assert.equal(saved[0].file,file);assert.equal(saved[0].published,undefined);assert.equal(saved[1].file.name,'other.txt');
});

test('People file ownership guards validate inputs and preserve legacy and replacement behavior', async () => {
 const store=createPeopleFileStore(fakeDriver()),id='0123456789abcdef',other='fedcba9876543210',file={name:'saved.txt',type:'text/plain',size:1};
 let token=await store.save('scope',id,file);await store.save('scope',other,{...file,name:'other.txt'});
 for (const bad of [null,false,0,{}]) {
  await assert.rejects(store.markPublishedBatch('scope',[id],7,[token],bad),/ownership/i);
  await assert.rejects(store.removeStageFile('scope',id,token,bad),/ownership/i);
 }
 for (const guard of [()=>false,()=>undefined,()=>{throw Error('guard failed');}]) {
  await assert.rejects(store.markPublishedBatch('scope',[id],7,[token],guard));
  await assert.rejects(store.removeStageFile('scope',id,token,guard));
  assert.equal((await store.list('scope')).length,2);
 }
 await store.markPublishedBatch('scope',[id],7,[token],()=>true);assert.equal((await store.list('scope'))[0].published,7);
 await store.removeStageFile('scope',id,token,()=>true);token=await store.save('scope',id,{...file,name:'replacement.txt'});
 const old='1'.repeat(8)+'-'+'1'.repeat(4)+'-'+'1'.repeat(4)+'-'+'1'.repeat(4)+'-'+'1'.repeat(12);
 await assert.rejects(store.markPublishedBatch('scope',[id],8,[old],()=>true),/conflict/i);
 await assert.rejects(store.removeStageFile('scope',id,old,()=>true),/conflict/i);
 const rows=await store.list('scope');assert.equal(rows.length,2);assert.equal(rows.find(r=>r.id===id).file.name,'replacement.txt');assert.equal(rows.find(r=>r.id===other).file.name,'other.txt');
 await store.markPublishedBatch('scope',[id],8,[token]);await store.removeStageFile('scope',id,token);assert.equal((await store.list('scope')).length,1);
});

test('People IndexedDB updates check ownership after a delayed database open', async () => {
 for(const action of ['mark','remove']) for(const initial of [false,true]) {
  const db=fakeIndexedDB(),original=createPeopleFileStore(createIndexedDBPeopleDriver(db)),id='0123456789abcdef',file={name:'saved.txt',type:'text/plain',size:2};
  const token=await original.save('scope',id,file);let release,current=initial;
  const delayed={open(){const request={};release=()=>{request.result=db.db;request.onsuccess();};return request;}};
  const store=createPeopleFileStore(createIndexedDBPeopleDriver(delayed));
  const operation=action==='mark'?store.markPublishedBatch('scope',[id],7,[token],()=>current):store.removeStageFile('scope',id,token,()=>current);
  const rejected=assert.rejects(operation,/cancel/i);assert.equal(typeof release,'function');current=false;release();await rejected;
  const rows=await original.list('scope');assert.equal(rows.length,1);assert.equal(rows[0].file,file);assert.equal(rows[0].published,undefined);
 }
});

test('People IndexedDB upgrades existing files and recovery together through the same version',async()=>{
 const file={name:'persisted.txt',type:'text/plain',size:2},id='0123456789abcdef',backing=fakeIndexedDB({legacyRows:[{key:'scope\0'+id,scope:'scope',id,file}]}),versions=[],indexedDB={open(name,version){versions.push([name,version]);return backing.open(name,version);}};
 const first=createPeopleFileStore(createIndexedDBPeopleDriver(indexedDB));
 assert.equal((await first.list('scope'))[0].file.name,'persisted.txt');
 const next=createPeopleFileStore(createIndexedDBPeopleDriver(indexedDB));
 assert.equal((await next.list('scope'))[0].file.name,'persisted.txt');
 assert.deepEqual(versions,[['scimux-people-file-drafts',2],['scimux-people-file-drafts',2]]);
 assert.equal(backing.db.objectStoreNames.contains('files'),true);
 assert.equal(backing.db.objectStoreNames.contains('recovery'),true);
});

test('failed People recovery upgrade aborts the upgrade transaction before retry',async()=>{
 let calls=0,aborts=0,closed=0;const good=fakeIndexedDB();
 const indexedDB={open(...args){if(++calls>1)return good.open(...args);const request={};queueMicrotask(()=>{request.result={objectStoreNames:{contains:name=>name==='files'},createObjectStore(){throw Error('synthetic upgrade failure');},close(){closed++;}};request.transaction={abort(){aborts++;}};request.onupgradeneeded();request.onsuccess();});return request;}};
 const store=createPeopleFileStore(createIndexedDBPeopleDriver(indexedDB));
 await assert.rejects(store.list('scope'),/unavailable/i);assert.equal(aborts,1);assert.equal(closed,1);
 await store.save('scope','0123456789abcdef',{name:'saved.txt',type:'text/plain',size:1});assert.equal((await store.list('scope'))[0].file.name,'saved.txt');assert.equal(calls,2);
});

test('a fresh People recovery store initializes its epoch and counter during upgrade',async()=>{
 const previous=Object.getOwnPropertyDescriptor(globalThis,'crypto'),backing=fakeIndexedDB();
 try{Object.defineProperty(globalThis,'crypto',{configurable:true,value:{getRandomValues:bytes=>bytes.fill(0x2a)}});
  const store=createPeopleFileStore(createIndexedDBPeopleDriver(backing));await store.list('scope');
  assert.deepEqual(backing.recovery.get('allocator'),{key:'allocator',epoch:'2a'.repeat(16),next:1});
 }finally{if(previous)Object.defineProperty(globalThis,'crypto',previous);else delete globalThis.crypto;}
});

test('People draft identity is returned only after commit and an aborted allocation can retry',async()=>{
 let rows=[{key:'allocator',epoch:'a'.repeat(32),next:1}],release,attempt=0;
 const driver={read:async()=>[],readWrite:async fn=>fn([]).value,writePeople:fn=>new Promise((resolve,reject)=>{const result=fn({files:[],recovery:rows.map(row=>({...row}))});release=()=>{if(++attempt===1)reject(Error('synthetic abort'));else{rows=result.recovery;resolve(result.value);}};})};
 const store=createPeopleFileStore(driver),save=()=>store.savePeopleDraft?.({scope:'copy',text:'synthetic draft'}),first=save();
 assert.ok(first,'the feature must persist draft versions');let settled=false;const observed=first.finally(()=>{settled=true;});await Promise.resolve();assert.equal(settled,false);release();await assert.rejects(observed,/abort/);assert.equal(rows[0].next,1);
 const second=save();release();const result=await second;assert.ok(result.version);assert.equal(rows[0].next>1,true);
});

test('People draft versions survive reopening the shared IndexedDB connection',async()=>{
 const indexedDB=fakeIndexedDB(),first=createPeopleFileStore(createIndexedDBPeopleDriver(indexedDB));
 const saved=await first.savePeopleDraft({scope:'synthetic-copy',text:'saved People caption'});
 const second=createPeopleFileStore(createIndexedDBPeopleDriver(indexedDB));
 const later=await second.savePeopleDraft({scope:'synthetic-copy',text:'new caption',head:saved.head,expected:saved.version});
 assert.notEqual(later.version,saved.version);assert.equal(later.head,saved.head);
 assert.equal(indexedDB.recovery.get('head:'+saved.head).version,later.version);
});

test('People file admissions include recovery and file metadata in the shared 256 MiB budget',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),file={name:'synthetic.txt',type:'text/plain',size:4*1024*1024};
 for(let i=0;i<63;i++)await store.save('scope-'+Math.floor(i/4),i.toString(16).padStart(16,'0'),file);
 await assert.rejects(store.save('scope-15',(63).toString(16).padStart(16,'0'),file),/limit/i);
 assert.equal((await store.list('scope-15')).length,3);assert.equal(db.recovery.has('allocator'),true);
});

test('People draft allocations do not reuse reclaimed versions and preserve newer edits',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db));
 const first=await store.savePeopleDraft({scope:'scope',text:'same'});
 const second=await store.savePeopleDraft({scope:'scope',text:'same',head:first.head,expected:first.version});
 assert.notEqual(first.version,second.version);assert.equal(db.recovery.has('draft:'+first.version),false);
 const separate=await store.savePeopleDraft({scope:'scope',text:'older',head:first.head,expected:first.version});assert.notEqual(separate.head,first.head);assert.equal(db.recovery.get('head:'+second.head).version,second.version);
 const third=await store.savePeopleDraft({scope:'scope',text:'same',head:second.head,expected:second.version});
 assert.notEqual(third.version,first.version);assert.notEqual(third.version,second.version);assert.equal(db.recovery.get('head:'+first.head).version,third.version);
});

for(const damaged of ['missing','epoch','counter','exhausted'])test(`People recovery fails closed with ${damaged} allocator state`,async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db));const original=await store.savePeopleDraft({scope:'scope',text:'saved'}),meta=db.recovery.get('allocator');
 if(damaged==='missing')db.recovery.delete('allocator');else db.recovery.set('allocator',{...meta,...(damaged==='epoch'?{epoch:'bad'}:{next:damaged==='counter'?0:Number.MAX_SAFE_INTEGER})});
 await assert.rejects(store.savePeopleDraft({scope:'scope',text:'new'}),/identity/i);assert.equal(db.recovery.get('head:'+original.head).version,original.version);assert.equal(db.recovery.get('draft:'+original.version).text,'saved');
});

test('reopening People storage without edits never allocates draft heads or identities',async()=>{
 const db=fakeIndexedDB(),initial=createPeopleFileStore(createIndexedDBPeopleDriver(db));await initial.list('scope');const before={...db.recovery.get('allocator')};
 for(let i=0;i<40;i++)assert.deepEqual(await createPeopleFileStore(createIndexedDBPeopleDriver(db)).list('scope'),[]);
 assert.deepEqual(db.recovery.get('allocator'),before);assert.equal(db.recovery.size,1);
});

test('People publication admission freezes the exact draft and saved file identities',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),file={name:'synthetic.txt',type:'text/plain',size:4};
 const token=await store.save(scope,id,file);await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'});
 const result=await store.beginPeoplePublication?.({scope,peopleKey:recoveryPeopleKey,head:draft.head,version:draft.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request:{key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]});
 assert.ok(result?.publication);const row=db.recovery.get('publication:'+result.publication);assert.equal(row.version,draft.version);assert.deepEqual(row.files,[{id,fileToken:token,stageRevision:revision}]);assert.equal(row.request.key,'2'.repeat(16));assert.equal(row.peopleKey,recoveryPeopleKey);assert.equal((await store.list(scope))[0].file,file);
});

test('a retired People publication fences a later saved-file mark from another graph',async()=>{
 const db=fakeIndexedDB(),first=createPeopleFileStore(createIndexedDBPeopleDriver(db)),second=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),file={name:'synthetic.txt',type:'text/plain',size:4};
 const token=await first.save(scope,id,file);await first.setStageRevision(scope,id,revision,token);const draft=await first.savePeopleDraft({scope,text:'caption'});
 const {publication}=await first.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,head:draft.head,version:draft.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request:{key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]});
 const key='publication:'+publication;db.recovery.set(key,{...db.recovery.get(key),state:'retired'});
 await assert.rejects(second.markPublishedBatch(scope,[id],7,[token],()=>true,publication),/retired|cancel/i);
 const row=(await first.list(scope))[0];assert.equal(row.published,undefined);assert.equal(row.file,file);assert.equal(row.fileToken,token);
});

test('editing after People publication admission preserves the captured draft version',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),file={name:'synthetic.txt',type:'text/plain',size:4};
 const token=await store.save(scope,id,file);await store.setStageRevision(scope,id,revision,token);const first=await store.savePeopleDraft({scope,text:'same'});
 const {publication}=await store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,head:first.head,version:first.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request:{key:'2'.repeat(16),text:'same',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]});
 const later=await store.savePeopleDraft({scope,text:'same',head:first.head,expected:first.version});
 assert.notEqual(later.version,first.version);assert.ok(db.recovery.has('draft:'+first.version));assert.equal(db.recovery.get('draft:'+first.version).text,'same');assert.equal(db.recovery.get('publication:'+publication).version,first.version);assert.equal(db.recovery.get('head:'+first.head).version,later.version);
});

test('completed People bookkeeping marks proven files but preserves a newer same-text draft',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),file={name:'synthetic.txt',type:'text/plain',size:4};
 const token=await store.save(scope,id,file);await store.setStageRevision(scope,id,revision,token);const first=await store.savePeopleDraft({scope,text:'same'});
 const {publication}=await store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,head:first.head,version:first.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request:{key:'2'.repeat(16),text:'same',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]});
 const later=await store.savePeopleDraft({scope,text:'same',head:first.head,expected:first.version});
 const result=await store.decidePeoplePublication?.({scope,publication,expected:recoveryExpected(db,publication),proof:{state:'completed',seq:7,text:'same',stage_ids:[id]}});
 assert.deepEqual(result,{state:'completed',seq:7,filesConfirmed:true,draftCleared:false});assert.equal((await store.list(scope))[0].published,7);assert.equal((await store.list(scope))[0].file,file);assert.equal(db.recovery.get('head:'+first.head).version,later.version);assert.equal(db.recovery.get('draft:'+later.version).text,'same');assert.equal(db.recovery.get('decision:'+publication).state,'completed');
});

test('older People completion keeps a draft referenced by a newer attachment-only publication',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),file={name:'synthetic.txt',type:'text/plain',size:4};
 const token=await store.save(scope,id,file);await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'same'});
 const input={scope,peopleKey:recoveryPeopleKey,head:draft.head,version:draft.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request:{key:'2'.repeat(16),text:'same',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]};
 const old=await store.beginPeoplePublication(input),newer=await store.beginPeoplePublication({...input,request:{...input.request,key:'3'.repeat(16)}});
 await store.decidePeoplePublication({scope,publication:old.publication,expected:recoveryExpected(db,old.publication),proof:{state:'completed',seq:7,text:'same',stage_ids:[id]}});
 assert.equal(db.recovery.get('publication:'+newer.publication).state,'pending');assert.ok(db.recovery.has('head:'+draft.head));assert.equal(db.recovery.get('head:'+draft.head).version,draft.version);assert.equal(db.recovery.get('draft:'+draft.version).text,'same');assert.equal((await store.list(scope))[0].file,file);assert.equal((await store.list(scope))[0].published,undefined);
});

test('draft growth cannot consume the completion record reserved for an admitted publication',async()=>{
 const rows=[{key:'allocator',epoch:'a'.repeat(32),next:1},...Array.from({length:2044},(_,i)=>({key:'decision:'+i,kind:'decision'})),{key:'publication:old',kind:'publication',state:'pending',scope:'other',version:'old'}];
 const driver={read:async()=>[],readWrite:async fn=>fn([]).value,writePeople:async fn=>fn({files:[],recovery:rows}).value},store=createPeopleFileStore(driver);
 await assert.rejects(store.savePeopleDraft({scope:'new',text:'draft'}),/limit/i);assert.equal(rows.length,2046);assert.equal(rows[0].next,1);
});

test('People recovery lists durable draft heads and pending publications after reload',async()=>{
 const db=fakeIndexedDB(),first=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),file={name:'synthetic.txt',type:'text/plain',size:4};
 const token=await first.save(scope,id,file);await first.setStageRevision(scope,id,revision,token);const draft=await first.savePeopleDraft({scope,text:'caption'});
 const {publication}=await first.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,head:draft.head,version:draft.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request:{key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]});
 const reopened=createPeopleFileStore(createIndexedDBPeopleDriver(db)),snapshot=await reopened.listPeopleRecovery?.(scope);
 assert.equal(snapshot?.heads.length,1);assert.equal(snapshot.heads[0].text,'caption');assert.equal(snapshot.heads[0].version,draft.version);assert.equal(snapshot.pending.length,1);assert.equal(snapshot.pending[0].publication,publication);assert.deepEqual(snapshot.pending[0].files,[{id,fileToken:token,stageRevision:revision}]);
});

test('a queued People draft write cannot create a head after its feature lifetime ends',async()=>{
 let records=[{key:'allocator',epoch:'a'.repeat(32),next:1}],release,current=true;
 const driver={read:async()=>[],readWrite:async fn=>fn([]).value,writePeople:fn=>new Promise((resolve,reject)=>{release=()=>{try{const result=fn({files:[],recovery:records});records=result.recovery;resolve(result.value);}catch(error){reject(error);}};})};
 const store=createPeopleFileStore(driver),operation=store.savePeopleDraft({scope:'scope',text:'old',current:()=>current}),rejected=assert.rejects(operation,/cancel/i);current=false;release();await rejected;assert.deepEqual(records,[{key:'allocator',epoch:'a'.repeat(32),next:1}]);
});

test('counter exhaustion prevents new identities but leaves saved People recovery readable',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),saved=await store.savePeopleDraft({scope:'scope',text:'saved'});db.recovery.set('allocator',{...db.recovery.get('allocator'),next:Number.MAX_SAFE_INTEGER});
 await assert.rejects(store.savePeopleDraft({scope:'scope',text:'new'}),/identity/i);const state=await store.listPeopleRecovery('scope');assert.equal(state.heads[0].version,saved.version);assert.equal(state.heads[0].text,'saved');
});

test('a queued People publication admission cannot outlive the captured composer flight',async()=>{
 const scope='scope',id='0123456789abcdef',token='11111111-1111-1111-1111-111111111111',revision='a'.repeat(64),version='a'.repeat(32)+':1';let release,current=true;
 const files=[{key:scope+'\0'+id,scope,id,file:{name:'saved.txt',type:'text/plain',size:1},fileToken:token,stageRevision:revision}],recovery=[{key:'allocator',epoch:'a'.repeat(32),next:2},{key:'head:'+version,scope,version},{key:'draft:'+version,scope,text:'caption'}];
 const driver={read:async()=>files,readWrite:async fn=>fn(files).value,writePeople:fn=>new Promise((resolve,reject)=>{release=()=>{try{resolve(fn({files,recovery}).value);}catch(error){reject(error);}};})},store=createPeopleFileStore(driver);
 const operation=store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,head:version,version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request:{key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}],current:()=>current}),rejected=assert.rejects(operation,/cancel/i);current=false;release();await rejected;assert.equal(recovery.length,3);assert.equal(recovery[0].next,2);
});

test('completed People bookkeeping preserves replacement file and unproven draft',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),file={name:'old.txt',type:'text/plain',size:4};
 const token=await store.save(scope,id,file);await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'});
 const {publication}=await store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,head:draft.head,version:draft.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request:{key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]});
 await store.remove(scope,id);const replacement={name:'replacement.txt',type:'text/plain',size:3};const replacementToken=await store.save(scope,id,replacement);await store.setStageRevision(scope,id,revision,replacementToken);
 const result=await store.decidePeoplePublication({scope,publication,expected:recoveryExpected(db,publication),proof:{state:'completed',seq:7,text:'caption',stage_ids:[id]}});
 assert.deepEqual(result,{state:'completed',seq:7,filesConfirmed:false,draftCleared:false});const row=(await store.list(scope))[0];assert.equal(row.file,replacement);assert.notEqual(row.fileToken,token);assert.equal(row.published,undefined);assert.equal(db.recovery.get('head:'+draft.head).version,draft.version);assert.equal(db.recovery.get('draft:'+draft.version).text,'caption');
});

test('retired People publication permits a new explicit publication without altering saved files',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),file={name:'saved.txt',type:'text/plain',size:4};
 const token=await store.save(scope,id,file);await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'});
 const input={scope,peopleKey:recoveryPeopleKey,head:draft.head,version:draft.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request:{key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]},old=await store.beginPeoplePublication(input);
 assert.deepEqual(await store.decidePeoplePublication({scope,publication:old.publication,expected:recoveryExpected(db,old.publication),proof:{state:'locally_discarded'}}),{state:'retired'});
 const next=await store.beginPeoplePublication({...input,request:{...input.request,key:'3'.repeat(16)}});assert.notEqual(next.publication,old.publication);assert.equal(db.recovery.get('publication:'+next.publication).state,'pending');assert.equal(db.recovery.get('head:'+draft.head).version,draft.version);const row=(await store.list(scope))[0];assert.equal(row.file,file);assert.equal(row.published,undefined);
});

test('queued People bookkeeping cannot commit after its original feature is destroyed',async()=>{
 const db=fakeIndexedDB(),base=createIndexedDBPeopleDriver(db),store=createPeopleFileStore(base),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),file={name:'saved.txt',type:'text/plain',size:4};
 const token=await store.save(scope,id,file);await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'});
 const {publication}=await store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,head:draft.head,version:draft.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request:{key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]});
 let release,current=true;const delayed=createPeopleFileStore({read:base.read,readWrite:base.readWrite,writePeople:fn=>new Promise((resolve,reject)=>{release=()=>base.writePeople(fn).then(resolve,reject);})});
 const operation=delayed.decidePeoplePublication({scope,publication,expected:recoveryExpected(db,publication),proof:{state:'locally_discarded'},current:()=>current}),rejected=assert.rejects(operation,/cancel/i);current=false;release();await rejected;assert.equal(db.recovery.get('publication:'+publication).state,'pending');assert.equal((await store.list(scope))[0].file,file);
});

test('publication-scoped saved-file writes require matching active identity and preserve replacements',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),file={name:'saved.txt',type:'text/plain',size:4};
 const token=await store.save(scope,id,file);await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'});
 const {publication}=await store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,head:draft.head,version:draft.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request:{key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]});
 await assert.rejects(store.markPublishedBatch(scope,[id],7,[token],()=>true,'bad'),/identity/i);
 await assert.rejects(store.markPublishedBatch('other',[id],7,[token],()=>true,publication),/retired|unavailable/i);
 await assert.rejects(store.markPublishedBatch(scope,[id],7,['22222222-2222-2222-2222-222222222222'],()=>true,publication),/conflict/i);
 await store.markPublishedBatch(scope,[id],7,[token],()=>true,publication);assert.equal((await store.list(scope))[0].published,7);
 await store.removeStageFile(scope,id,token,()=>true,publication);assert.deepEqual(await store.list(scope),[]);
});

test('People recovery rejects invalid input, unavailable drivers and mismatched publication proof',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),empty=createPeopleFileStore(fakeDriver());
 for(const input of [{scope:'',text:'x'},{scope:'scope',text:null},{scope:'scope',text:'x',head:'h'},{scope:'scope',text:'x',current:null}])await assert.rejects(store.savePeopleDraft(input),/identity/i);
 await assert.rejects(empty.savePeopleDraft({scope:'scope',text:'x'}),/unavailable/i);await assert.rejects(empty.listPeopleRecovery('scope'),/unavailable/i);
 await assert.rejects(store.beginPeoplePublication({scope:'bad'}),/identity/i);
 await assert.rejects(empty.decidePeoplePublication({scope:'scope',publication:'a'.repeat(32)+':1',expected:{},proof:{state:'locally_discarded'}}),/unavailable/i);
 await assert.rejects(store.decidePeoplePublication({scope:'scope',publication:'bad',proof:{state:'completed'}}),/decision/i);
 const id='0123456789abcdef',token=await store.save('scope',id,{name:'saved.txt',type:'text/plain',size:1}),revision='a'.repeat(64);await store.setStageRevision('scope',id,revision,token);const draft=await store.savePeopleDraft({scope:'scope',text:'caption'});
 const input={scope:'scope',peopleKey:recoveryPeopleKey,head:draft.head,version:draft.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request:{key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]};
 await assert.rejects(store.beginPeoplePublication({...input,version:'other'}),/changed/i);
 await assert.rejects(store.beginPeoplePublication({...input,files:[{...input.files[0],fileToken:'22222222-2222-2222-2222-222222222222'}]}),/conflict/i);
 const {publication}=await store.beginPeoplePublication(input);await assert.rejects(store.beginPeoplePublication(input),/conflict/i);
 await assert.rejects(store.decidePeoplePublication({scope:'scope',publication,expected:recoveryExpected(db,publication),proof:{state:'completed',seq:7,text:'different',stage_ids:[id]}}),/changed/i);
 assert.equal(db.recovery.get('publication:'+publication).state,'pending');assert.equal((await store.list('scope'))[0].fileToken,token);
});

test('confirmed People completion reclaims only its unreferenced draft version',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),file={name:'saved.txt',type:'text/plain',size:4};
 const token=await store.save(scope,id,file);await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'}),other=await store.savePeopleDraft({scope:'other',text:'other caption'});
 const {publication}=await store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,head:draft.head,version:draft.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request:{key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]});
 await store.decidePeoplePublication({scope,publication,expected:recoveryExpected(db,publication),proof:{state:'completed',seq:7,text:'caption',stage_ids:[id]}});
 assert.equal(db.recovery.has('head:'+draft.head),false);assert.equal(db.recovery.has('draft:'+draft.version),false);assert.equal(db.recovery.get('draft:'+other.version).text,'other caption');assert.equal((await store.list(scope))[0].file,file);
});

test('confirmed People draft clearing preserves a newer version and other heads',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),old=await store.savePeopleDraft({scope:'scope',text:'old'}),newer=await store.savePeopleDraft({scope:'scope',head:old.head,expected:old.version,text:'newer'}),other=await store.savePeopleDraft({scope:'other',text:'other'});
 assert.equal(await store.clearPeopleDraft?.({scope:'scope',head:old.head,version:old.version}),false);assert.equal(db.recovery.get('head:'+newer.head).version,newer.version);
 assert.equal(await store.clearPeopleDraft?.({scope:'scope',head:newer.head,version:newer.version}),true);assert.equal(db.recovery.has('head:'+newer.head),false);assert.equal(db.recovery.has('draft:'+newer.version),false);assert.equal(db.recovery.get('draft:'+other.version).text,'other');
});

test('a later People edit forks safely if confirmed cleanup already removed its old head',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),first=await store.savePeopleDraft({scope:'scope',text:'first'});
 assert.equal(await store.clearPeopleDraft({scope:'scope',head:first.head,version:first.version}),true);
 const newer=await store.savePeopleDraft({scope:'scope',head:first.head,expected:first.version,text:'newer'});
 assert.notEqual(newer.head,first.head);assert.notEqual(newer.version,first.version);assert.equal(db.recovery.get('draft:'+newer.version).text,'newer');
});

test('a People edit based on a changed head preserves both graphs as separate drafts',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),old=await store.savePeopleDraft({scope:'scope',text:'old'}),newer=await store.savePeopleDraft({scope:'scope',head:old.head,expected:old.version,text:'newer'});
 const delayed=await store.savePeopleDraft({scope:'scope',head:old.head,expected:old.version,text:'separate'});
 assert.notEqual(delayed.head,newer.head);const heads=(await store.listPeopleRecovery('scope')).heads;assert.deepEqual(heads.map(head=>head.text).sort(),['newer','separate']);
});

test('a durable legacy People retirement preserves the original mutable browser slot',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='legacy-scope',copyInstance='1'.repeat(32),binding='binding',publicationKey='2'.repeat(16),id='3'.repeat(16),value=JSON.stringify({instance:'',copyInstance,request:{text:'legacy caption',anchor:0,key:publicationKey,attachments:[id]}}),storageKey=`scimux-people-file-pending:${recoveryPeopleKey}::${binding}:copy:${copyInstance}`,raw=new Map([[storageKey,value]]),snapshot={key:recoveryPeopleKey,copyInstance,binding,instance:'',storageKey,value,publicationKey,text:'legacy caption',attachments:[id]};
 const result=await store.recordLegacyPeopleDecision?.({scope,snapshot,proof:{state:'locally_discarded'}});
 assert.equal(result?.state,'retired');assert.equal(raw.get(storageKey),value);const state=await store.listPeopleRecovery(scope);assert.equal(state.legacy.length,1);assert.equal(state.legacy[0].snapshot.value,value);assert.equal(state.legacy[0].snapshot.publicationKey,publicationKey);
});

test('confirmed People cleanup removes only a proven completed file with no newer pending user',async()=>{
 for(const newer of [false,true]){
  const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),file={name:'saved.txt',type:'text/plain',size:4},token=await store.save(scope,id,file);await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'});
  const input={scope,peopleKey:recoveryPeopleKey,head:draft.head,version:draft.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request:{key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]},old=await store.beginPeoplePublication(input);
  if(newer)await store.beginPeoplePublication({...input,request:{...input.request,key:'3'.repeat(16)}});
  const done=await store.decidePeoplePublication({scope,publication:old.publication,expected:recoveryExpected(db,old.publication),proof:{state:'completed',seq:7,text:'caption',stage_ids:[id]}});
  if(newer){assert.equal(done.filesConfirmed,false);await assert.rejects(store.removeStageFile(scope,id,token,()=>true,old.publication),/conflict|retired/i);assert.equal((await store.list(scope))[0].file,file);}else{assert.equal(done.filesConfirmed,true);await store.removeStageFile(scope,id,token,()=>true,old.publication);assert.deepEqual(await store.list(scope),[]);}
 }
});

test('a People publication cannot reuse a file already confirmed sent',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),file={name:'saved.txt',type:'text/plain',size:4},token=await store.save(scope,id,file);await store.setStageRevision(scope,id,revision,token);await store.markPublishedBatch(scope,[id],7,[token]);const draft=await store.savePeopleDraft({scope,text:'caption'});
 await assert.rejects(store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,head:draft.head,version:draft.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request:{key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]}),/sent|conflict/i);const row=(await store.list(scope))[0];assert.equal(row.file,file);assert.equal(row.published,7);assert.equal(db.recovery.get('head:'+draft.head).version,draft.version);
});

test('People recovery inventories pending publications from prior bindings of the same copy',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),copyInstance='1'.repeat(32),id='0123456789abcdef',revision='a'.repeat(64),publications=[];
 for(const binding of ['old','current']){const scope=`people\0${recoveryPeopleKey}\0binding:${binding}\0copy:${copyInstance}\0`,token=await store.save(scope,id,{name:binding+'.txt',type:'text/plain',size:1});await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:binding});const result=await store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,head:draft.head,version:draft.version,copyInstance,binding,native:'',request:{key:(binding==='old'?'2':'3').repeat(16),text:binding,anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]});publications.push(result.publication);}
 const entries=await store.listRetainedPeopleRecovery?.({peopleKey:recoveryPeopleKey,copyInstance,native:''});assert.ok(entries);assert.deepEqual(entries.map(row=>row.publication).sort(),publications.sort());assert.deepEqual(entries.map(row=>row.binding).sort(),['current','old']);
});

test('a People retirement decision cannot target a different reviewed publication key',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),token=await store.save(scope,id,{name:'saved.txt',type:'text/plain',size:1});await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'}),request={key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},input={scope,peopleKey:recoveryPeopleKey,head:draft.head,version:draft.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request,files:[{id,fileToken:token,stageRevision:revision}]},saved=await store.beginPeoplePublication(input);
 await assert.rejects(store.decidePeoplePublication({scope,publication:saved.publication,expected:{key:recoveryPeopleKey,copyInstance:input.copyInstance,binding:input.binding,instance:'',head:draft.head,version:draft.version,publicationKey:'9'.repeat(16),text:request.text,attachments:request.attachments,value:JSON.stringify({instance:'',copyInstance:input.copyInstance,request:{...request,key:'9'.repeat(16)}})},proof:{state:'locally_discarded'}}),/identity|changed/i);assert.equal(db.recovery.get('publication:'+saved.publication).state,'pending');assert.equal((await store.list(scope))[0].published,undefined);
});

test('uncertain text-only People publication survives recovery reload with its exact request',async()=>{
 const db=fakeIndexedDB(),first=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='text-scope',draft=await first.savePeopleDraft({scope,text:'synthetic text'}),request={key:'2'.repeat(16),text:'synthetic text',anchor:3},copyInstance='1'.repeat(32);
 const admitted=await first.beginPeopleTextPublication?.({scope,peopleKey:recoveryPeopleKey,copyInstance,binding:'binding',native:'',head:draft.head,version:draft.version,request});assert.ok(admitted?.publication);
 const reloaded=createPeopleFileStore(createIndexedDBPeopleDriver(db)),state=await reloaded.listPeopleRecovery(scope);assert.equal(state.textPending.length,1);assert.deepEqual(state.textPending[0].request,request);assert.equal(state.textPending[0].publication,admitted.publication);assert.equal(state.heads[0].version,draft.version);
});

test('editing while a text-only People publication is uncertain preserves its frozen draft',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',old=await store.savePeopleDraft({scope,text:'caption'}),request={key:'2'.repeat(16),text:'caption',anchor:0},saved=await store.beginPeopleTextPublication({scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',head:old.head,version:old.version,request});
 const newer=await store.savePeopleDraft({scope,text:'newer',head:old.head,expected:old.version});assert.ok(db.recovery.has('draft:'+old.version));assert.equal(db.recovery.get('text-publication:'+saved.publication).version,old.version);assert.equal(db.recovery.get('head:'+old.head).version,newer.version);
});

test('confirmed text-only People publication completes only its exact request and newer draft survives',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',old=await store.savePeopleDraft({scope,text:'caption'}),request={key:'2'.repeat(16),text:'caption',anchor:0},input={scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',head:old.head,version:old.version,request},saved=await store.beginPeopleTextPublication(input),newer=await store.savePeopleDraft({scope,text:'caption',head:old.head,expected:old.version});
 const result=await store.finishPeopleTextPublication?.({scope,publication:saved.publication,expected:input,seq:7});assert.deepEqual(result,{state:'completed',seq:7,draftCleared:false});assert.equal(db.recovery.get('head:'+old.head).version,newer.version);assert.equal(db.recovery.get('text-decision:'+saved.publication).state,'completed');assert.equal((await store.listPeopleRecovery(scope)).textPending.length,0);
});

test('file completion cannot clear a draft still referenced by an uncertain text-only publication',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),token=await store.save(scope,id,{name:'saved.txt',type:'text/plain',size:1});await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'}),request={key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},file=await store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,head:draft.head,version:draft.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request,files:[{id,fileToken:token,stageRevision:revision}]}),text=await store.beginPeopleTextPublication({scope,peopleKey:recoveryPeopleKey,head:draft.head,version:draft.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request:{key:'3'.repeat(16),text:'caption',anchor:0}});
 const result=await store.decidePeoplePublication({scope,publication:file.publication,expected:recoveryExpected(db,file.publication),proof:{state:'completed',seq:7,text:'caption',stage_ids:[id]}});assert.equal(result.draftCleared,false);assert.equal(db.recovery.get('head:'+draft.head).version,draft.version);assert.equal(db.recovery.get('text-publication:'+text.publication).state,'pending');
});

test('file publication admission preserves the reservation of an uncertain text-only publication',async()=>{
 const scope='scope',id='0123456789abcdef',token='11111111-1111-1111-1111-111111111111',revision='a'.repeat(64),version='a'.repeat(32)+':1',files=[{key:scope+'\0'+id,scope,id,file:{name:'saved.txt',type:'text/plain',size:1},fileToken:token,stageRevision:revision}],recovery=[{key:'allocator',epoch:'a'.repeat(32),next:2},{key:'head:'+version,kind:'head',scope,head:version,version},{key:'draft:'+version,kind:'draft',scope,version,text:'caption'},...Array.from({length:2042},(_,i)=>({key:'decision:'+i,kind:'decision'})),{key:'text-publication:old',kind:'text-publication',state:'pending',scope:'other',version:'old'}],driver={read:async()=>files,readWrite:async fn=>fn(files).value,writePeople:async fn=>fn({files,recovery}).value},store=createPeopleFileStore(driver);
 await assert.rejects(store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,head:version,version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request:{key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]}),/limit/i);assert.equal(recovery[0].next,2);assert.equal(recovery.at(-1).state,'pending');
});

test('completed People publication with unproven replacement remains reviewable with actual sequence',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),token=await store.save(scope,id,{name:'old.txt',type:'text/plain',size:1});await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'}),request={key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},saved=await store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,head:draft.head,version:draft.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request,files:[{id,fileToken:token,stageRevision:revision}]});await store.remove(scope,id);const replacement=await store.save(scope,id,{name:'new.txt',type:'text/plain',size:2});await store.setStageRevision(scope,id,revision,replacement);
 const decision=await store.decidePeoplePublication({scope,publication:saved.publication,expected:recoveryExpected(db,saved.publication),proof:{state:'completed',seq:7,text:'caption',stage_ids:[id]}});assert.equal(decision.filesConfirmed,false);const state=await store.listPeopleRecovery(scope);assert.equal(state.completed.length,1);assert.equal(state.completed[0].seq,7);assert.equal(state.completed[0].filesConfirmed,false);assert.equal(state.completed[0].request.text,'caption');assert.equal((await store.list(scope))[0].file.name,'new.txt');
});

test('legacy People decisions are idempotent, reject changed proof, and keep all raw values',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',copyInstance='1'.repeat(32),binding='binding',id='3'.repeat(16),publicationKey='2'.repeat(16),value=JSON.stringify({instance:'',copyInstance,request:{text:'legacy',anchor:0,key:publicationKey,attachments:[id]}}),storageKey=`scimux-people-file-pending:${recoveryPeopleKey}::${binding}:copy:${copyInstance}`,snapshot={key:recoveryPeopleKey,copyInstance,binding,instance:'',storageKey,value,publicationKey,text:'legacy',attachments:[id]},proof={state:'completed',seq:7,text:'legacy',stage_ids:[id]};
 assert.deepEqual(await store.recordLegacyPeopleDecision({scope,snapshot,proof}),{state:'completed'});const after=db.recovery.get('allocator').next;
 assert.deepEqual(await store.recordLegacyPeopleDecision({scope,snapshot,proof}),{state:'completed'});assert.equal(db.recovery.get('allocator').next,after);
 await assert.rejects(store.recordLegacyPeopleDecision({scope,snapshot,proof:{...proof,seq:8}}),/decision changed/i);
 await assert.rejects(store.recordLegacyPeopleDecision({scope,snapshot,proof:{state:'locally_discarded'}}),/decision changed/i);
 await assert.rejects(store.recordLegacyPeopleDecision({scope,snapshot,proof:{...proof,text:'other'}}),/review changed/i);
 await assert.rejects(store.recordLegacyPeopleDecision({scope,snapshot:{...snapshot,storageKey:'other'},proof}),/review changed/i);
 await assert.rejects(store.recordLegacyPeopleDecision({scope,snapshot,proof,current:()=>false}),/cancel/i);
 const saved=(await store.listPeopleRecovery(scope)).legacy;assert.equal(saved.length,1);assert.equal(saved[0].seq,7);assert.equal(saved[0].snapshot.value,value);
});

test('People text publication rejects stale, duplicate, unavailable and cancelled admissions',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),empty=createPeopleFileStore(fakeDriver()),scope='scope',draft=await store.savePeopleDraft({scope,text:'caption'}),request={text:'caption',anchor:0,key:'2'.repeat(16)},input={scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',head:draft.head,version:draft.version,request};
 await assert.rejects(store.beginPeopleTextPublication({...input,current:null}),/unavailable/i);await assert.rejects(empty.beginPeopleTextPublication(input),/unavailable/i);await assert.rejects(store.beginPeopleTextPublication({...input,current:()=>false}),/cancel/i);await assert.rejects(store.beginPeopleTextPublication({...input,version:'other'}),/changed/i);
 const saved=await store.beginPeopleTextPublication(input);await assert.rejects(store.beginPeopleTextPublication(input),/unconfirmed/i);
 await assert.rejects(store.finishPeopleTextPublication({scope,publication:saved.publication,expected:{...input,request:{...request,key:'3'.repeat(16)}},seq:7}),/identity/i);
 await assert.rejects(store.finishPeopleTextPublication({scope,publication:saved.publication,expected:input,seq:7,current:()=>false}),/cancel/i);
 assert.deepEqual(await store.finishPeopleTextPublication({scope,publication:saved.publication,expected:input,seq:7}),{state:'completed',seq:7,draftCleared:true});assert.equal(db.recovery.has('head:'+draft.head),false);
});

test('a People publication decision never repurposes the immutable pending record',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),token=await store.save(scope,id,{name:'saved.txt',type:'text/plain',size:1});await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'}),request={key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},saved=await store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,head:draft.head,version:draft.version,copyInstance:'1'.repeat(32),binding:'binding',native:'',request,files:[{id,fileToken:token,stageRevision:revision}]});
 const original=structuredClone(db.recovery.get('publication:'+saved.publication));await store.decidePeoplePublication({scope,publication:saved.publication,expected:recoveryExpected(db,saved.publication),proof:{state:'locally_discarded'}});
 assert.equal(db.recovery.has('publication:'+saved.publication),false);const decision=db.recovery.get('decision:'+saved.publication);assert.equal(decision.state,'retired');assert.deepEqual(decision.request,original.request);assert.deepEqual(decision.files,original.files);assert.equal((await store.list(scope))[0].fileToken,token);
});

test('a People text completion records its decision under a distinct immutable key',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',draft=await store.savePeopleDraft({scope,text:'caption'}),request={key:'2'.repeat(16),text:'caption',anchor:0},input={scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',head:draft.head,version:draft.version,request},saved=await store.beginPeopleTextPublication(input);
 const original=structuredClone(db.recovery.get('text-publication:'+saved.publication));await store.finishPeopleTextPublication({scope,publication:saved.publication,expected:input,seq:7});
 assert.equal(db.recovery.has('text-publication:'+saved.publication),false);const decision=db.recovery.get('text-decision:'+saved.publication);assert.equal(decision.state,'completed');assert.equal(decision.seq,7);assert.deepEqual(decision.request,original.request);
});

test('a conflicting People text decision cannot change a pending publication or draft',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',draft=await store.savePeopleDraft({scope,text:'caption'}),request={key:'2'.repeat(16),text:'caption',anchor:0},input={scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',head:draft.head,version:draft.version,request},saved=await store.beginPeopleTextPublication(input),key='text-decision:'+saved.publication;
 db.recovery.set(key,{key,kind:'text-decision',state:'completed',seq:3});await assert.rejects(store.finishPeopleTextPublication({scope,publication:saved.publication,expected:input,seq:7}),/identity/i);assert.equal(db.recovery.get(key).seq,3);assert.equal(db.recovery.get('text-publication:'+saved.publication).state,'pending');assert.equal(db.recovery.get('head:'+draft.head).version,draft.version);
});

test('People transaction failure is reported only after rollback finishes',async()=>{
 let transaction;const db={objectStoreNames:{contains:()=>true},close(){},transaction(){const tx=transaction={objectStore(){return {getAll(){return {};}};}};return tx;}},indexedDB={open(){const request={};queueMicrotask(()=>{request.result=db;request.onsuccess();});return request;}},store=createPeopleFileStore(createIndexedDBPeopleDriver(indexedDB));let result='pending';const operation=store.listPeopleRecovery('scope').then(()=>{result='success';},()=>{result='failed';});await new Promise(resolve=>setImmediate(resolve));transaction.onerror?.({});await new Promise(resolve=>setImmediate(resolve));assert.equal(result,'pending');transaction.onabort?.({});await operation;assert.equal(result,'failed');
});

test('frozen People draft forks once for another writer and clears only the submitted head',async()=>{
 const db=fakeIndexedDB(),first=createPeopleFileStore(createIndexedDBPeopleDriver(db)),second=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='synthetic-copy';
 const original=await first.savePeopleDraft({scope,text:'submitted'});const frozen=await first.freezePeopleDraft?.({scope,text:'submitted',...original});
 assert.ok(frozen,'submission must be durably frozen');assert.notEqual(frozen.head,original.head);assert.notEqual(frozen.version,original.version);
 assert.equal(db.recovery.get('head:'+original.head).version,original.version);assert.equal(db.recovery.get('draft:'+frozen.version).text,'submitted');assert.equal(db.recovery.get('head:'+frozen.head).scope,scope);
 const newer=await second.savePeopleDraft({scope,text:'submitted',head:frozen.head,expected:frozen.version});assert.notEqual(newer.head,frozen.head);
 let latest=newer;for(let i=0;i<5;i++)latest=await second.savePeopleDraft({scope,text:'edit '+i,head:latest.head,expected:latest.version});
 assert.equal(latest.head,newer.head);assert.equal((await first.listPeopleRecovery(scope)).heads.length,3);
 const request={key:'2'.repeat(16),text:'submitted',anchor:0},identity={scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',...frozen,request},pending=await first.beginPeopleTextPublication(identity);
 await first.finishPeopleTextPublication({scope,publication:pending.publication,expected:identity,seq:7});const heads=(await second.listPeopleRecovery(scope)).heads;assert.deepEqual(heads.map(row=>row.text),['edit 4']);assert.equal(heads[0].head,latest.head);
});

test('freezing an already advanced People head preserves the other writer and captured text',async()=>{
 const db=fakeIndexedDB(),first=createPeopleFileStore(createIndexedDBPeopleDriver(db)),second=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='synthetic-copy';
 const original=await first.savePeopleDraft({scope,text:'submitted'});const newer=await second.savePeopleDraft({scope,text:'newer',head:original.head,expected:original.version});
 const frozen=await first.freezePeopleDraft?.({scope,text:'submitted',...original});assert.ok(frozen,'captured text must be frozen despite a stale head');assert.notEqual(frozen.head,newer.head);
 const heads=(await first.listPeopleRecovery(scope)).heads;assert.deepEqual(heads.map(row=>row.text).sort(),['newer','submitted']);assert.equal(db.recovery.get('head:'+newer.head).version,newer.version);
});

test('independent People freezes respect cancellation and capacity',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='synthetic-copy',original=await store.savePeopleDraft({scope,text:'submitted'});
 const beforeCancel={...db.recovery.get('allocator')};
 await assert.rejects(async()=>{if(!store.freezePeopleDraft)throw Error('freeze unavailable');return store.freezePeopleDraft({scope,text:'submitted',...original,current:()=>false});},/cancelled/);
 assert.deepEqual(db.recovery.get('allocator'),beforeCancel);
 const frozen=await store.freezePeopleDraft({scope,text:'submitted',...original}),other=await store.freezePeopleDraft({scope,text:'submitted',...original});assert.notEqual(frozen.head,other.head);assert.notEqual(frozen.version,other.version);assert.equal(db.recovery.get('draft:'+frozen.version).text,'submitted');assert.equal(db.recovery.get('draft:'+other.version).text,'submitted');
 while((await store.listPeopleRecovery(scope)).heads.length<32)await store.savePeopleDraft({scope,text:'other'});
 const beforeFull={...db.recovery.get('allocator')};
 await assert.rejects(store.freezePeopleDraft({scope,text:'stale',head:original.head,version:'stale'}),/limit/i);
 assert.deepEqual(db.recovery.get('allocator'),beforeFull);assert.equal((await store.listPeopleRecovery(scope)).heads.length,32);assert.equal(db.recovery.get('draft:'+original.version).text,'submitted');assert.equal(db.recovery.get('draft:'+frozen.version).text,'submitted');assert.equal(db.recovery.get('draft:'+other.version).text,'submitted');
});

test('People store teardown closes its owned open database without deleting saved bytes',async()=>{
 const db=fakeIndexedDB();let closed=0;db.db.close=()=>{closed++;};
 const store=createBrowserPeopleFileStore(db);await store.save('synthetic-owned','0123456789abcdef',{name:'synthetic.txt',type:'text/plain',size:1});
 store.destroy?.();assert.equal(closed,1,'teardown must close the database connection owned by this store');
 store.destroy?.();assert.equal(closed,1,'teardown must be idempotent');
 const fresh=createBrowserPeopleFileStore(db);assert.equal((await fresh.list('synthetic-owned'))[0].file.name,'synthetic.txt');fresh.destroy?.();
});

test('People store retirement before open never opens and refuses later operations',async()=>{
 let opens=0;const backing=fakeIndexedDB(),store=createBrowserPeopleFileStore({open(...args){opens++;return backing.open(...args);}});
 store.destroy?.();store.destroy?.();assert.equal(opens,0);await assert.rejects(store.list('scope'),/unavailable|closed|retired/i);assert.equal(opens,0);
});

test('People store retirement closes a late open and refuses its pending write',async()=>{
 const backing=fakeIndexedDB();let request,closed=0;backing.db.close=()=>closed++;
 const store=createBrowserPeopleFileStore({open(){request={};return request;}}),pending=store.save('scope','0123456789abcdef',{name:'synthetic.txt',type:'text/plain',size:1});
 store.destroy?.();request.result=backing.db;request.onsuccess();await assert.rejects(pending,/unavailable|closed|retired/i);assert.equal(closed,1);assert.equal((await createBrowserPeopleFileStore(backing).list('scope')).length,0);
});

test('People store retirement closes only its own connection and leaves another usable',async()=>{
 const backing=fakeIndexedDB(),connections=[];const indexedDB={open(...args){const request=backing.open(...args);let result;Object.defineProperty(request,'result',{get(){return result;},set(db){const connection=Object.create(db);connection.closed=0;connection.close=()=>connection.closed++;connections.push(connection);result=connection;}});return request;}};
 const first=createBrowserPeopleFileStore(indexedDB),second=createBrowserPeopleFileStore(indexedDB);await first.list('scope');await second.list('scope');assert.equal(connections.length,2);
 first.destroy?.();assert.equal(connections[0].closed,1);assert.equal(connections[1].closed,0);await second.save('scope','0123456789abcdef',{name:'synthetic.txt',type:'text/plain',size:1});assert.equal((await second.list('scope')).length,1);second.destroy?.();assert.equal(connections[1].closed,1);
});

test('an earlier People publication completion cannot erase a later frozen submission',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='synthetic-copy',draft=await store.savePeopleDraft({scope,text:'same text'}),request={key:'2'.repeat(16),text:'same text',anchor:0},identity={scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',...draft,request};
 const earlier=await store.beginPeopleTextPublication(identity),frozen=await store.freezePeopleDraft({scope,text:request.text,...draft});
 await store.finishPeopleTextPublication({scope,publication:earlier.publication,expected:identity,seq:7});
 const remaining=await store.listPeopleRecovery(scope);assert.ok(remaining.heads.some(row=>row.head===frozen.head&&row.version===frozen.version&&row.text===request.text),'earlier completion must preserve the later explicit submission');
});

test('independent People submissions freeze distinct heads and versions for the same text',async()=>{
 const db=fakeIndexedDB(),first=createPeopleFileStore(createIndexedDBPeopleDriver(db)),second=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='synthetic-copy';
 const original=await first.savePeopleDraft({scope,text:'same text'});
 const a=await first.freezePeopleDraft({scope,text:'same text',...original});const b=await second.freezePeopleDraft({scope,text:'same text',...original});
 assert.notEqual(a.head,original.head);assert.notEqual(a.version,original.version);assert.notEqual(b.head,original.head);assert.notEqual(b.version,original.version);assert.notEqual(a.head,b.head);assert.notEqual(a.version,b.version);
 const heads=(await first.listPeopleRecovery(scope)).heads;assert.equal(heads.length,3);assert.ok(heads.every(row=>row.text==='same text'));
});

test('a blocked People database open closes a later successful connection after retirement',async()=>{
 let request,closed=0;const store=createBrowserPeopleFileStore({open(){request={};return request;}}),pending=store.list('scope');request.onblocked();await assert.rejects(pending,/unavailable/i);store.destroy();request.result={close(){closed++;}};request.onsuccess();assert.equal(closed,1);await assert.rejects(store.list('scope'),/unavailable/i);
});

test('People database version changes close the old connection once and retirement closes its replacement',async()=>{
 const backing=fakeIndexedDB();let closed=0;backing.db.close=()=>closed++;const store=createBrowserPeopleFileStore(backing);await store.list('scope');backing.db.onversionchange();assert.equal(closed,1);await store.list('scope');store.destroy();store.destroy();assert.equal(closed,2);await assert.rejects(store.list('scope'),/unavailable/i);
});

test('retirement allows an already started People read to complete or abort',async()=>{
 for(const outcome of ['complete','abort']){
  let tx,request,started;const ready=new Promise(resolve=>{started=resolve;}),db={close(){},transaction(){tx={objectStore(){return {getAll(){request={};started();return request;}};}};return tx;}},indexedDB={open(){const req={};queueMicrotask(()=>{req.result=db;req.onsuccess();});return req;}},store=createBrowserPeopleFileStore(indexedDB);
  const pending=store.list('scope');await ready;store.destroy();if(outcome==='complete'){request.result=[];request.onsuccess();tx.oncomplete();assert.deepEqual(await pending,[]);}else{tx.onabort();await assert.rejects(pending,/unavailable/i);}await assert.rejects(store.list('scope'),/unavailable/i);
 }
});

test('People writes already started at retirement commit or abort without deleting prior bytes',async()=>{
 for(const outcome of ['complete','abort']){
  const backing=fakeIndexedDB(),initial=createBrowserPeopleFileStore(backing);await initial.save('scope','0123456789abcdef',{name:'prior.txt',type:'text/plain',size:1});initial.destroy();
  const transaction=backing.db.transaction;let started,tx;const ready=new Promise(resolve=>{started=resolve;});backing.db.transaction=function(...args){tx=transaction.apply(this,args);started();return tx;};
  const store=createBrowserPeopleFileStore(backing),pending=store.save('scope','fedcba9876543210',{name:'new.txt',type:'text/plain',size:1});await ready;store.destroy();if(outcome==='abort'){tx.abort();await assert.rejects(pending,/unavailable/i);}else await pending;
  backing.db.transaction=transaction;const fresh=createBrowserPeopleFileStore(backing);const rows=await fresh.list('scope');assert.ok(rows.some(row=>row.file.name==='prior.txt'));assert.equal(rows.some(row=>row.file.name==='new.txt'),outcome==='complete');fresh.destroy();
 }
});

test('confirmed People text submission atomically clears only its unchanged source draft',async()=>{
 for(const edit of ['none','same','different']){
  const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='synthetic-copy',original=await store.savePeopleDraft({scope,text:'submitted'}),frozen=await store.freezePeopleDraft({scope,text:'submitted',...original});
  assert.notEqual(frozen.head,original.head);assert.notEqual(frozen.version,original.version);
  const request={key:'2'.repeat(16),text:'submitted',anchor:0},identity={scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',...frozen,request},pending=await store.beginPeopleTextPublication(identity);
  let newer;if(edit!=='none')newer=await store.savePeopleDraft({scope,text:edit==='same'?'submitted':'newer',head:original.head,expected:original.version});
  await store.finishPeopleTextPublication({scope,publication:pending.publication,expected:identity,seq:7});const heads=(await store.listPeopleRecovery(scope)).heads;
  if(edit==='none')assert.deepEqual(heads,[]);else{assert.equal(heads.length,1);assert.equal(heads[0].head,newer.head);assert.equal(heads[0].version,newer.version);assert.equal(heads[0].text,edit==='same'?'submitted':'newer');}
 }
});

test('confirmed People submission keeps a source head protected by another pending publication',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='synthetic-copy',original=await store.savePeopleDraft({scope,text:'submitted'}),request={key:'2'.repeat(16),text:'submitted',anchor:0};
 const firstIdentity={scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',...original,request},earlier=await store.beginPeopleTextPublication(firstIdentity),frozen=await store.freezePeopleDraft({scope,text:'submitted',...original});
 assert.notEqual(frozen.head,original.head);await store.finishPeopleTextPublication({scope,publication:earlier.publication,expected:firstIdentity,seq:7});
 const rows=await store.listPeopleRecovery(scope);assert.ok(rows.heads.some(row=>row.head===frozen.head&&row.version===frozen.version));
});

test('confirmed People file submission clears its own snapshot and only unchanged source',async()=>{
 for(const edit of ['none','same','different']){
  const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='synthetic-copy',id='0123456789abcdef',revision='a'.repeat(64),token=await store.save(scope,id,{name:'synthetic.txt',type:'text/plain',size:1});await store.setStageRevision(scope,id,revision,token);
  const original=await store.savePeopleDraft({scope,text:'submitted'}),frozen=await store.freezePeopleDraft({scope,text:'submitted',...original}),request={key:'2'.repeat(16),text:'submitted',anchor:0,attachments:[id]};assert.notEqual(frozen.head,original.head);assert.notEqual(frozen.version,original.version);const pending=await store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',...frozen,request,files:[{id,fileToken:token,stageRevision:revision}]});
  let newer;if(edit!=='none')newer=await store.savePeopleDraft({scope,text:edit==='same'?'submitted':'newer',head:original.head,expected:original.version});
  const result=await store.decidePeoplePublication({scope,publication:pending.publication,expected:recoveryExpected(db,pending.publication),proof:{state:'completed',seq:7,text:'submitted',stage_ids:[id]}});assert.equal(result.filesConfirmed,true);assert.equal((await store.list(scope))[0].published,7);
  const heads=(await store.listPeopleRecovery(scope)).heads;if(edit==='none')assert.deepEqual(heads,[]);else{assert.equal(heads.length,1);assert.equal(heads[0].head,newer.head);assert.equal(heads[0].version,newer.version);assert.equal(heads[0].text,edit==='same'?'submitted':'newer');}
 }
});

test('a frozen People file submission survives earlier file publication completion',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='synthetic-copy',id='0123456789abcdef',revision='a'.repeat(64),token=await store.save(scope,id,{name:'synthetic.txt',type:'text/plain',size:1});await store.setStageRevision(scope,id,revision,token);
 const original=await store.savePeopleDraft({scope,text:'submitted'}),request={key:'2'.repeat(16),text:'submitted',anchor:0,attachments:[id]},pending=await store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',...original,request,files:[{id,fileToken:token,stageRevision:revision}]}),frozen=await store.freezePeopleDraft({scope,text:'submitted',...original});
 await store.decidePeoplePublication({scope,publication:pending.publication,expected:recoveryExpected(db,pending.publication),proof:{state:'completed',seq:7,text:'submitted',stage_ids:[id]}});const heads=(await store.listPeopleRecovery(scope)).heads;assert.ok(heads.some(row=>row.head===frozen.head&&row.version===frozen.version&&row.text==='submitted'));
});

test('People submission freeze refuses oversized source identity without allocating storage',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='synthetic-copy',original=await store.savePeopleDraft({scope,text:'submitted'}),before={...db.recovery.get('allocator')};
 await assert.rejects(store.freezePeopleDraft({scope,text:'submitted',head:'x'.repeat(70000),version:original.version}),/identity|limit/i);assert.deepEqual(db.recovery.get('allocator'),before);assert.equal((await store.listPeopleRecovery(scope)).heads.length,1);assert.equal(db.recovery.get('draft:'+original.version).text,'submitted');
});

test('People store retirement settles an already pending open without a later platform event',async()=>{
 let request,closed=0,outcome='pending';const store=createBrowserPeopleFileStore({open(){request={};return request;}}),pending=store.list('synthetic-scope').then(()=>{outcome='success';},()=>{outcome='unavailable';});
 try{assert.ok(request);store.destroy?.();await new Promise(resolve=>setImmediate(resolve));assert.equal(outcome,'unavailable','retirement must reject pending work even if the platform open event never arrives');}
 finally{request.result={close(){closed++;}};request.onsuccess();await pending;assert.equal(closed,1,'a later connection is closed exactly once');}
});

test('People completion preserves an unchanged source protected by another pending publication',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='synthetic-copy',id='0123456789abcdef',revision='a'.repeat(64),token=await store.save(scope,id,{name:'synthetic.txt',type:'text/plain',size:1});await store.setStageRevision(scope,id,revision,token);
 const original=await store.savePeopleDraft({scope,text:'same'}),first=await store.freezePeopleDraft({scope,text:'same',...original}),second=await store.freezePeopleDraft({scope,text:'same',...original}),base={scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:''},text={...base,...first,request:{key:'2'.repeat(16),text:'same',anchor:0}},message=await store.beginPeopleTextPublication(text),file=await store.beginPeoplePublication({...base,...second,request:{key:'3'.repeat(16),text:'same',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]});
 await store.finishPeopleTextPublication({scope,publication:message.publication,expected:text,seq:7});const heads=(await store.listPeopleRecovery(scope)).heads;assert.ok(heads.some(row=>row.head===original.head&&row.version===original.version));assert.ok(heads.some(row=>row.head===second.head&&row.version===second.version));assert.equal(db.recovery.get('publication:'+file.publication).state,'pending');assert.equal((await store.list(scope))[0].fileToken,token);
});

test('confirmed frozen People file submission cleans its text while preserving a replacement file',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='synthetic-copy',id='0123456789abcdef',revision='a'.repeat(64),token=await store.save(scope,id,{name:'original.txt',type:'text/plain',size:1});await store.setStageRevision(scope,id,revision,token);
 const original=await store.savePeopleDraft({scope,text:'submitted'}),frozen=await store.freezePeopleDraft({scope,text:'submitted',...original}),request={key:'2'.repeat(16),text:'submitted',anchor:0,attachments:[id]},pending=await store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',...frozen,request,files:[{id,fileToken:token,stageRevision:revision}]});
 await store.remove(scope,id);const replacement=await store.save(scope,id,{name:'replacement.txt',type:'text/plain',size:1});const result=await store.decidePeoplePublication({scope,publication:pending.publication,expected:recoveryExpected(db,pending.publication),proof:{state:'completed',seq:7,text:'submitted',stage_ids:[id]}});assert.equal(result.filesConfirmed,false);assert.equal((await store.list(scope))[0].fileToken,replacement);assert.equal((await store.listPeopleRecovery(scope)).heads.length,0,'confirmed frozen input and unchanged source must be cleared atomically');
});

test('People freeze rejects missing recovery support, invalid allocators and collisions without changing saved text',async()=>{
 const unavailable=createPeopleFileStore({read:async()=>[],readWrite:async fn=>fn([]).value});await assert.rejects(unavailable.freezePeopleDraft({scope:'scope',text:'saved',head:'head',version:'version'}),/unavailable/i);
 for(const mode of ['missing','epoch','counter','collision-head','collision-draft']){
  const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),saved=await store.savePeopleDraft({scope:'scope',text:'saved'}),allocator=db.recovery.get('allocator');
  if(mode==='missing')db.recovery.delete('allocator');else if(mode==='epoch')db.recovery.set('allocator',{...allocator,epoch:'invalid'});else if(mode==='counter')db.recovery.set('allocator',{...allocator,next:Number.MAX_SAFE_INTEGER});else{const id=allocator.epoch+':'+allocator.next;db.recovery.set((mode==='collision-head'?'head:':'draft:')+id,{key:(mode==='collision-head'?'head:':'draft:')+id});}
  await assert.rejects(store.freezePeopleDraft({scope:'scope',text:'saved',...saved}),/identity|conflict/i);assert.equal(db.recovery.get('draft:'+saved.version).text,'saved');assert.equal(db.recovery.get('head:'+saved.head).version,saved.version);
 }
});

test('People retirement between open success and transaction start prevents reads and writes',async()=>{
 for(const mode of ['read','write']){
  let request,closed=0,transactions=0;const db={close(){closed++;},transaction(){transactions++;throw Error('must not start');}},store=createBrowserPeopleFileStore({open(){request={};return request;}});
  const operation=mode==='read'?store.list('scope'):store.save('scope','0123456789abcdef',{name:'synthetic.txt',type:'text/plain',size:1});request.result=db;request.onsuccess();store.destroy();await assert.rejects(operation,/unavailable/i);assert.equal(closed,1);assert.equal(transactions,0);
 }
});

test('People draft and publication operations reject damaged or cross-scope recovery without mutation',async()=>{
 const key=recoveryPeopleKey,copyInstance='1'.repeat(32),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),token=await store.save(scope,id,{name:'saved.txt',type:'text/plain',size:1});await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'saved'}),request={key:'2'.repeat(16),text:'saved',anchor:0,attachments:[id]},fileInput={scope,peopleKey:key,copyInstance,binding:'binding',native:'',...draft,request,files:[{id,fileToken:token,stageRevision:revision}]},textInput={...fileInput,request:{key:'3'.repeat(16),text:'saved',anchor:0}};
 await assert.rejects(store.savePeopleDraft({scope:'other',text:'new',head:draft.head,expected:draft.version}),/changed/i);
 const allocator=db.recovery.get('allocator'),nextID=allocator.epoch+':'+allocator.next;
 db.recovery.set('draft:'+nextID,{key:'draft:'+nextID});await assert.rejects(store.savePeopleDraft({scope,text:'new'}),/conflict/i);db.recovery.delete('draft:'+nextID);
 db.recovery.set('oversized',{key:'oversized',data:'x'.repeat(65536)});await assert.rejects(store.savePeopleDraft({scope,text:'new'}),/limit/i);db.recovery.delete('oversized');
 const empty=createPeopleFileStore(fakeDriver());await assert.rejects(empty.beginPeoplePublication(fileInput),/unavailable/i);await assert.rejects(empty.finishPeopleTextPublication({scope,publication:nextID,expected:{request:{}},seq:1}),/unavailable/i);await assert.rejects(empty.listRetainedPeopleRecovery({peopleKey:key,copyInstance,native:''}),/unavailable/i);
 for(const [kind,input] of [['file',fileInput],['text',textInput]]){
  db.recovery.delete('allocator');await assert.rejects(kind==='file'?store.beginPeoplePublication(input):store.beginPeopleTextPublication(input),/identity/i);db.recovery.set('allocator',allocator);
  const collision=(kind==='file'?'publication:':'text-publication:')+nextID;db.recovery.set(collision,{key:collision});await assert.rejects(kind==='file'?store.beginPeoplePublication(input):store.beginPeopleTextPublication(input),/conflict/i);db.recovery.delete(collision);
 }
 const bad='draft:'+draft.version,prior=db.recovery.get(bad);db.recovery.delete(bad);await assert.rejects(store.listPeopleRecovery(scope),/unavailable/i);db.recovery.set(bad,prior);assert.equal((await store.list(scope))[0].fileToken,token);assert.equal(db.recovery.get('head:'+draft.head).version,draft.version);
 await assert.rejects(store.clearPeopleDraft({scope,...draft,current:()=>false}),/cancel/i);await assert.rejects(store.markPublishedBatch(scope,[id],1,[token],()=>true,'a'.repeat(32)+':999'),/retired|unavailable/i);
});

test('People stage removal rejects a file shared by another pending publication',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),token=await store.save(scope,id,{name:'saved.txt',type:'text/plain',size:1});await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'}),base={scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',...draft,files:[{id,fileToken:token,stageRevision:revision}]},request={key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},first=await store.beginPeoplePublication({...base,request});await store.beginPeoplePublication({...base,request:{...request,key:'3'.repeat(16)}});await store.markPublishedBatch(scope,[id],7,[token]);await assert.rejects(store.removeStageFile(scope,id,token,()=>true,first.publication),/conflict/i);assert.equal((await store.list(scope))[0].fileToken,token);
});

test('People confirmation never clears a source head whose text differs from the submitted capture',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='synthetic-copy',original=await store.savePeopleDraft({scope,text:'original unsent draft'}),frozen=await store.freezePeopleDraft({scope,text:'different submitted text',...original}),request={key:'2'.repeat(16),text:'different submitted text',anchor:0},identity={scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',...frozen,request},saved=await store.beginPeopleTextPublication(identity);
 await store.finishPeopleTextPublication({scope,publication:saved.publication,expected:identity,seq:7});const heads=(await store.listPeopleRecovery(scope)).heads;assert.ok(heads.some(row=>row.head===original.head&&row.version===original.version&&row.text==='original unsent draft'),'confirmation must not clear a different source draft');
});

test('People publication admission preserves data on full pending capacity and decision conflicts',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',id='0123456789abcdef',revision='a'.repeat(64),token=await store.save(scope,id,{name:'saved.txt',type:'text/plain',size:1});await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'}),base={scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',...draft},request={key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},files=[{id,fileToken:token,stageRevision:revision}],first=await store.beginPeoplePublication({...base,request,files});
 await store.markPublishedBatch(scope,[id],8,[token]);await assert.rejects(store.decidePeoplePublication({scope,publication:first.publication,expected:recoveryExpected(db,first.publication),proof:{state:'completed',seq:7,text:'caption',stage_ids:[id]}}),/conflict/i);assert.equal((await store.list(scope))[0].published,8);
 const key='decision:'+first.publication;db.recovery.set(key,{key});await assert.rejects(store.decidePeoplePublication({scope,publication:first.publication,expected:recoveryExpected(db,first.publication),proof:{state:'locally_discarded'}}),/conflict/i);db.recovery.delete(key);
 const pending=db.recovery.get('publication:'+first.publication);for(let i=0;i<15;i++){const k='synthetic-pending:'+i;db.recovery.set(k,{...pending,key:k});}const textRequest={key:'3'.repeat(16),text:'caption',anchor:0};await assert.rejects(store.beginPeopleTextPublication({...base,request:textRequest}),/limit/i);assert.equal(db.recovery.get('publication:'+first.publication).state,'pending');assert.equal(db.recovery.get('draft:'+draft.version).text,'caption');
});

test('People legacy decisions reject unavailable identity, allocator collision and capacity',async()=>{
 const scope='scope',copyInstance='1'.repeat(32),binding='binding',id='3'.repeat(16),publicationKey='2'.repeat(16),value=JSON.stringify({instance:'',copyInstance,request:{text:'legacy',anchor:0,key:publicationKey,attachments:[id]}}),storageKey=`scimux-people-file-pending:${recoveryPeopleKey}::${binding}:copy:${copyInstance}`,snapshot={key:recoveryPeopleKey,copyInstance,binding,instance:'',storageKey,value,publicationKey,text:'legacy',attachments:[id]},proof={state:'locally_discarded'},empty=createPeopleFileStore(fakeDriver());
 await assert.rejects(empty.recordLegacyPeopleDecision({scope,snapshot:{...snapshot,copyInstance:undefined},proof}),/unavailable/i);await assert.rejects(empty.recordLegacyPeopleDecision({scope,snapshot,proof}),/unavailable/i);
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db));await store.list(scope);const allocator=db.recovery.get('allocator');db.recovery.delete('allocator');await assert.rejects(store.recordLegacyPeopleDecision({scope,snapshot,proof}),/identity/i);db.recovery.set('allocator',allocator);
 const key='legacy:'+allocator.epoch+':'+allocator.next;db.recovery.set(key,{key});await assert.rejects(store.recordLegacyPeopleDecision({scope,snapshot,proof}),/conflict/i);db.recovery.delete(key);
 for(let i=0;i<2048;i++)db.recovery.set('padding:'+i,{key:'padding:'+i});await assert.rejects(store.recordLegacyPeopleDecision({scope,snapshot,proof}),/limit/i);assert.deepEqual(db.recovery.get('allocator'),allocator);assert.equal(snapshot.value,value);
});

test('People indexed storage rejects failed write transactions without committing data',async()=>{
 for(const mode of ['transaction','object-store','get-all']){
  let tx,aborts=0,requests=0;const db={close(){},transaction(){if(mode==='transaction')throw Error('synthetic failure');tx={abort(){aborts++;queueMicrotask(()=>tx.onabort());},objectStore(){if(mode==='object-store')throw Error('synthetic failure');return {getAll(){const request={};if(++requests===1)queueMicrotask(()=>request.onerror());return request;}};}};return tx;}},indexedDB={open(){const req={};queueMicrotask(()=>{req.result=db;req.onsuccess();});return req;}},store=createBrowserPeopleFileStore(indexedDB);
  await assert.rejects(store.save('scope','0123456789abcdef',{name:'saved.txt',type:'text/plain',size:1}),/unavailable/i);assert.equal(aborts,mode==='transaction'?0:1);store.destroy();
 }
});

test('People database upgrade creates missing files without replacing existing recovery',async()=>{
 let created=[],tx,closed=0;const db={objectStoreNames:{contains:name=>name==='recovery'},createObjectStore(name){created.push(name);return {};},close(){closed++;},transaction(){tx={objectStore(){return {getAll(){const req={};queueMicrotask(()=>{req.result=[];req.onsuccess();tx.oncomplete();});return req;}};}};return tx;}},indexedDB={open(){const req={};queueMicrotask(()=>{req.result=db;req.onupgradeneeded();req.onsuccess();});return req;}},store=createBrowserPeopleFileStore(indexedDB);
 assert.deepEqual(await store.list('scope'),[]);assert.deepEqual(created,['files']);store.destroy();assert.equal(closed,1);
});

test('People confirmation cannot clear another unadmitted frozen submission used as its source',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='synthetic-copy',original=await store.savePeopleDraft({scope,text:'same'}),earlier=await store.freezePeopleDraft({scope,text:'same',...original}),later=await store.freezePeopleDraft({scope,text:'same',...earlier}),request={key:'2'.repeat(16),text:'same',anchor:0},identity={scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',...later,request},saved=await store.beginPeopleTextPublication(identity);
 await store.finishPeopleTextPublication({scope,publication:saved.publication,expected:identity,seq:7});const heads=(await store.listPeopleRecovery(scope)).heads;assert.ok(heads.some(row=>row.head===earlier.head&&row.version===earlier.version&&row.text==='same'),'another Send must not remove the earlier frozen input while it awaits admission');
});

test('generic People draft cleanup cannot remove an unadmitted frozen submission',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='synthetic-copy',original=await store.savePeopleDraft({scope,text:'submitted'}),frozen=await store.freezePeopleDraft({scope,text:'submitted',...original});assert.equal(await store.clearPeopleDraft({scope,...frozen}),false);assert.ok((await store.listPeopleRecovery(scope)).heads.some(row=>row.head===frozen.head&&row.version===frozen.version));
});

test('People freeze rejects oversized preexisting metadata and preserves existing drafts',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',draft=await store.savePeopleDraft({scope,text:'saved'}),allocator={...db.recovery.get('allocator')};db.recovery.set('oversized',{key:'oversized',data:'x'.repeat(65536)});await assert.rejects(store.freezePeopleDraft({scope,text:'saved',...draft}),/limit/i);assert.deepEqual(db.recovery.get('allocator'),allocator);assert.equal(db.recovery.get('draft:'+draft.version).text,'saved');
});

test('People draft clear respects a pending publication and missing driver',async()=>{
 const empty=createPeopleFileStore(fakeDriver());await assert.rejects(empty.clearPeopleDraft({scope:'scope',head:'head',version:'version'}),/unavailable/i);
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',draft=await store.savePeopleDraft({scope,text:'caption'}),request={text:'caption',anchor:0,key:'2'.repeat(16)},saved=await store.beginPeopleTextPublication({scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',...draft,request});assert.equal(await store.clearPeopleDraft({scope,...draft}),false);assert.equal(db.recovery.get('text-publication:'+saved.publication).state,'pending');assert.equal(db.recovery.get('draft:'+draft.version).text,'caption');
});

test('People legacy retirement accounts for an existing pending text publication',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='scope',copyInstance='1'.repeat(32),binding='binding',id='3'.repeat(16),publicationKey='2'.repeat(16),draft=await store.savePeopleDraft({scope,text:'caption'}),text=await store.beginPeopleTextPublication({scope,peopleKey:recoveryPeopleKey,copyInstance,binding,native:'',...draft,request:{text:'caption',anchor:0,key:'4'.repeat(16)}}),value=JSON.stringify({instance:'',copyInstance,request:{text:'legacy',anchor:0,key:publicationKey,attachments:[id]}}),storageKey=`scimux-people-file-pending:${recoveryPeopleKey}::${binding}:copy:${copyInstance}`,snapshot={key:recoveryPeopleKey,copyInstance,binding,instance:'',storageKey,value,publicationKey,text:'legacy',attachments:[id]};
 assert.deepEqual(await store.recordLegacyPeopleDecision({scope,snapshot,proof:{state:'locally_discarded'}}),{state:'retired'});assert.equal(db.recovery.get('text-publication:'+text.publication).state,'pending');assert.equal(snapshot.value,value);
});

test('People upgrade abort failure still closes its late connection',async()=>{
 let closed=0;const indexedDB={open(){const request={};queueMicrotask(()=>{request.result={objectStoreNames:{contains:()=>false},createObjectStore(){throw Error('synthetic upgrade failure');},close(){closed++;}};request.transaction={abort(){throw Error('already aborted');}};request.onupgradeneeded();request.onsuccess();});return request;}},store=createBrowserPeopleFileStore(indexedDB);await assert.rejects(store.list('scope'),/unavailable/i);assert.equal(closed,1);store.destroy();
});

test('People driver rejects a transaction that exceeds the metadata budget atomically',async()=>{
 const db=fakeIndexedDB(),driver=createIndexedDBPeopleDriver(db);await driver.read();const before={...db.recovery.get('allocator')};await assert.rejects(driver.writePeople(({files,recovery})=>({files,recovery:[...recovery,...Array.from({length:130},(_,i)=>({key:'padding:'+i,text:'x'.repeat(65500)}))],value:true})),/limit/i);assert.deepEqual(db.recovery.get('allocator'),before);assert.equal(db.recovery.size,1);driver.destroy();
});

test('People recovery refuses a missing allocator and a missing reviewed publication',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db));await store.list('scope');const allocator=db.recovery.get('allocator');db.recovery.delete('allocator');await assert.rejects(store.listPeopleRecovery('scope'),/identity/i);db.recovery.set('allocator',allocator);await assert.rejects(store.decidePeoplePublication({scope:'scope',publication:allocator.epoch+':999',expected:{},proof:{state:'locally_discarded'}}),/conflict/i);assert.deepEqual(db.recovery.get('allocator'),allocator);
});

test('aborted People completion retains both draft identities and pending publication after restart',async()=>{
 const db=fakeIndexedDB(),store=createPeopleFileStore(createIndexedDBPeopleDriver(db)),scope='synthetic-copy',original=await store.savePeopleDraft({scope,text:'caption'}),frozen=await store.freezePeopleDraft({scope,text:'caption',...original}),request={key:'2'.repeat(16),text:'caption',anchor:0},identity={scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',...frozen,request},saved=await store.beginPeopleTextPublication(identity),transaction=db.db.transaction;let aborted=0;
 db.db.transaction=function(...args){const tx=transaction.apply(this,args),objectStore=tx.objectStore;tx.objectStore=function(name){const result=objectStore.call(this,name);if(name==='recovery'){const add=result.add;result.add=row=>{if(row.key.startsWith('text-decision:')){aborted++;queueMicrotask(()=>tx.abort());return {};}return add.call(result,row);};}return result;};return tx;};
 await assert.rejects(store.finishPeopleTextPublication({scope,publication:saved.publication,expected:identity,seq:7}),/unavailable/i);assert.equal(aborted,1);store.destroy();db.db.transaction=transaction;const fresh=createPeopleFileStore(createIndexedDBPeopleDriver(db)),state=await fresh.listPeopleRecovery(scope);assert.equal(state.heads.length,2);assert.ok(state.heads.some(row=>row.head===original.head&&row.version===original.version));assert.ok(state.heads.some(row=>row.head===frozen.head&&row.version===frozen.version&&row.sealed));assert.equal(state.textPending[0].publication,saved.publication);assert.deepEqual(state.textPending[0].request,request);fresh.destroy();
});

test('retired capture remains protected by another pending publication using the same head',async()=>{
 const db=fakeIndexedDB(),store=createBrowserPeopleFileStore(db),scope='people\0synthetic\0',copyInstance='1'.repeat(32),id='3'.repeat(16),revision='a'.repeat(64),token=await store.save(scope,id,{name:'kept.txt',type:'text/plain',size:1});
 await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'saved'}),captured=await store.freezePeopleDraft({scope,text:'saved',...draft}),base={scope,peopleKey:recoveryPeopleKey,copyInstance,binding:'',native:'',...captured,files:[{id,fileToken:token,stageRevision:revision}]};
 const retired=await store.beginPeoplePublication({...base,request:{key:'2'.repeat(16),text:'saved',anchor:0,attachments:[id]}});await store.decidePeoplePublication({scope,publication:retired.publication,expected:recoveryExpected(db,retired.publication),proof:{state:'locally_discarded'}});
 const pending=await store.beginPeoplePublication({...base,request:{key:'4'.repeat(16),text:'saved',anchor:0,attachments:[id]}}),before=JSON.stringify([...db.recovery.values()]),files=await store.list(scope);
 assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),false);assert.equal(JSON.stringify([...db.recovery.values()]),before);assert.equal(db.recovery.get('publication:'+pending.publication).state,'pending');assert.deepEqual(await store.list(scope),files);
});

test('retired capture removal preserves an unrelated publication sharing its saved file',async()=>{
 const db=fakeIndexedDB(),store=createBrowserPeopleFileStore(db),scope='people\0synthetic\0',copyInstance='1'.repeat(32),id='3'.repeat(16),revision='a'.repeat(64),token=await store.save(scope,id,{name:'kept.txt',type:'text/plain',size:1});
 await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'saved'}),captured=await store.freezePeopleDraft({scope,text:'saved',...draft}),common={scope,peopleKey:recoveryPeopleKey,copyInstance,binding:'',native:'',files:[{id,fileToken:token,stageRevision:revision}]};
 const retired=await store.beginPeoplePublication({...common,...captured,request:{key:'2'.repeat(16),text:'saved',anchor:0,attachments:[id]}});await store.decidePeoplePublication({scope,publication:retired.publication,expected:recoveryExpected(db,retired.publication),proof:{state:'locally_discarded'}});
 const other=await store.savePeopleDraft({scope,text:'other'}),pending=await store.beginPeoplePublication({...common,...other,request:{key:'4'.repeat(16),text:'other',anchor:0,attachments:[id]}}),before=[...db.recovery.values()].map(row=>JSON.parse(JSON.stringify(row))),files=await store.list(scope);
 assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),true);assert.deepEqual([...db.recovery.values()],before.filter(row=>row.key!=='head:'+captured.head&&row.key!=='draft:'+captured.version));assert.equal(db.recovery.get('publication:'+pending.publication).state,'pending');assert.deepEqual(await store.list(scope),files);
});

for(const field of ['head','version','sourceHead','sourceVersion']) test(`captured draft refuses unknown metadata referencing its ${field}`,async()=>{
 const db=fakeIndexedDB(),store=createBrowserPeopleFileStore(db),scope='people\0synthetic\0',draft=await store.savePeopleDraft({scope,text:'saved'}),captured=await store.freezePeopleDraft({scope,text:'saved',...draft});
 db.recovery.set('unknown:reference',{key:'unknown:reference',kind:'future-supported-record',scope,[field]:field.toLowerCase().includes('version')?captured.version:captured.head});const before=JSON.stringify([...db.recovery.values()]);
 assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),false);assert.equal(JSON.stringify([...db.recovery.values()]),before);
});

async function textRetirementContractFixture() {
 const db=fakeIndexedDB(),store=createBrowserPeopleFileStore(db),scope='people\0synthetic\0',copyInstance='1'.repeat(32),draft=await store.savePeopleDraft({scope,text:'saved text'}),captured=await store.freezePeopleDraft({scope,text:'saved text',...draft});
 const expected={peopleKey:recoveryPeopleKey,copyInstance,binding:'binding',native:'',...captured,request:{key:'2'.repeat(16),text:'saved text',anchor:0}},saved=await store.beginPeopleTextPublication({scope,...expected});
 await store.save(scope,'3'.repeat(16),{name:'retained.txt',type:'text/plain',size:1});
 return {db,store,scope,expected,publication:saved.publication,draft,captured};
}

for(const field of ['peopleKey','copyInstance','binding','native','head','version','request.key','request.text','request.anchor','publication','scope']) for(const missing of [false,true]) test(`text retirement refuses ${missing?'missing':'wrong'} ${field} without mutation`,async()=>{
 const f=await textRetirementContractFixture(),expected=structuredClone(f.expected),args={scope:f.scope,publication:f.publication,expected,current:()=>true};
 const target=field.startsWith('request.')?expected.request:field==='publication'||field==='scope'?args:expected,key=field.startsWith('request.')?field.slice(8):field;
 if(missing)delete target[key];else target[key]=key==='anchor'?1:'wrong';
 const before=JSON.stringify([...f.db.recovery.values()]),files=await f.store.list(f.scope);
 assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');await assert.rejects(async()=>f.store.retirePeopleTextPublication(args));assert.equal(JSON.stringify([...f.db.recovery.values()]),before);assert.deepEqual(await f.store.list(f.scope),files);
});

test('explicit text retirement retains immutable identity, files, source and newer edits, and repeats idempotently',async()=>{
 const f=await textRetirementContractFixture(),newer=await f.store.savePeopleDraft({scope:f.scope,text:'newer',head:f.draft.head,expected:f.draft.version}),before=[...f.db.recovery.values()].map(row=>structuredClone(row)),files=await f.store.list(f.scope),args={scope:f.scope,publication:f.publication,expected:f.expected,current:()=>true};
 assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');const first=await f.store.retirePeopleTextPublication(args),second=await f.store.retirePeopleTextPublication(args);assert.equal(first.state,'retired');assert.deepEqual(second,first);
 const rows=[...f.db.recovery.values()],decision=rows.find(row=>row.kind==='text-decision'&&row.publication===f.publication),prior=before.find(row=>row.kind==='text-publication'&&row.publication===f.publication);
 assert.deepEqual(decision,{...prior,key:'text-decision:'+f.publication,kind:'text-decision',state:'retired'});assert.deepEqual(rows.filter(row=>row!==decision),before.filter(row=>row!==prior));assert.ok(rows.some(row=>row.key==='head:'+newer.head&&row.version===newer.version));assert.deepEqual(await f.store.list(f.scope),files);
 await assert.rejects(async()=>f.store.finishPeopleTextPublication({...args,seq:7}));assert.deepEqual([...f.db.recovery.values()],rows);
});

test('text retirement reports the completed sequence when completion wins',async()=>{
 const f=await textRetirementContractFixture(),args={scope:f.scope,publication:f.publication,expected:f.expected,current:()=>true};await f.store.finishPeopleTextPublication({...args,seq:7});const before=JSON.stringify([...f.db.recovery.values()]);
 assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');const result=await f.store.retirePeopleTextPublication(args);assert.equal(result.state,'completed');assert.equal(result.seq,7);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);
});

for(const condition of ['stale-current','missing-row','corrupt-row','stale-text']) test(`text retirement refuses ${condition} without modifying durable state`,async()=>{
 const f=await textRetirementContractFixture(),args={scope:f.scope,publication:f.publication,expected:structuredClone(f.expected),current:()=>condition!=='stale-current'},key='text-publication:'+f.publication;
 if(condition==='missing-row')f.db.recovery.delete(key);if(condition==='corrupt-row')f.db.recovery.set(key,{...f.db.recovery.get(key),request:null});if(condition==='stale-text')args.expected.request.text='previous review';
 const before=JSON.stringify([...f.db.recovery.values()]);assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');await assert.rejects(async()=>f.store.retirePeopleTextPublication(args));assert.equal(JSON.stringify([...f.db.recovery.values()]),before);
});

test('discarding an eligible captured draft frees one of the measured 32 scoped heads',async()=>{
 const db=fakeIndexedDB(),store=createBrowserPeopleFileStore(db),scope='people\0synthetic\0',draft=await store.savePeopleDraft({scope,text:'saved'}),captured=await store.freezePeopleDraft({scope,text:'saved',...draft});
 for(let i=0;i<30;i++)await store.savePeopleDraft({scope,text:'other '+i});assert.equal((await store.listPeopleRecovery(scope)).heads.length,32);
 await assert.rejects(store.savePeopleDraft({scope,text:'extra'}),/limit/i);assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),true);
 assert.equal((await store.listPeopleRecovery(scope)).heads.length,31);await store.savePeopleDraft({scope,text:'extra'});assert.equal((await store.listPeopleRecovery(scope)).heads.length,32);
});

test('discarding an eligible capture frees two measured recovery entries at the 2048 limit',async()=>{
 const db=fakeIndexedDB(),store=createBrowserPeopleFileStore(db),scope='people\0synthetic\0',draft=await store.savePeopleDraft({scope,text:'saved'}),captured=await store.freezePeopleDraft({scope,text:'saved',...draft});
 while(db.recovery.size<2048){const key='padding:'+db.recovery.size;db.recovery.set(key,{key,kind:'synthetic-padding'});}assert.equal(db.recovery.size,2048);
 await assert.rejects(store.savePeopleDraft({scope,text:'extra'}),/limit/i);assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),true);assert.equal(db.recovery.size,2046);
 await store.savePeopleDraft({scope,text:'extra'});assert.equal(db.recovery.size,2048);
});

test('discarding an eligible capture releases measured metadata bytes at the 8 MiB limit',async()=>{
 const db=fakeIndexedDB(),store=createBrowserPeopleFileStore(db),scope='people\0synthetic\0',draft=await store.savePeopleDraft({scope,text:'saved'}),captured=await store.freezePeopleDraft({scope,text:'saved',...draft}),encoder=new TextEncoder(),size=row=>encoder.encode(row.key).length+encoder.encode(JSON.stringify(row)).length,limit=8*1024*1024;
 const allocator=db.recovery.get('allocator'),version=allocator.epoch+':'+allocator.next,add=[{key:'draft:'+version,kind:'draft',scope,version,text:'x'},{key:'head:'+version,kind:'head',scope,head:version,version}],oldAllocator=size(allocator),newAllocator=size({...allocator,next:allocator.next+1});
 let bytes=[...db.recovery.values()].reduce((n,row)=>n+size(row),0),target=limit-add.reduce((n,row)=>n+size(row),0)-(newAllocator-oldAllocator)+1,index=0;
 while(bytes<target){const key='padding:'+index++,base={key,kind:'synthetic-padding',data:''},payload=Math.min(64000,target-bytes-size(base));assert.ok(payload>=0);const row={...base,data:'x'.repeat(payload)};db.recovery.set(key,row);bytes+=size(row);}
 assert.equal(bytes,target);await assert.rejects(store.savePeopleDraft({scope,text:'x'}),/limit/i);assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),true);await store.savePeopleDraft({scope,text:'x'});
 const measured=[...db.recovery.values()].reduce((n,row)=>n+size(row),0);assert.ok(measured<=limit);assert.ok(measured>limit-1024);
});

for(const change of ['scope','head','version','text','state']) test(`retired capture refuses a decision with mismatched ${change}`,async()=>{
 const db=fakeIndexedDB(),store=createBrowserPeopleFileStore(db),scope='people\0synthetic\0',copyInstance='1'.repeat(32),id='3'.repeat(16),revision='a'.repeat(64),token=await store.save(scope,id,{name:'kept.txt',type:'text/plain',size:1});await store.setStageRevision(scope,id,revision,token);
 const draft=await store.savePeopleDraft({scope,text:'saved'}),captured=await store.freezePeopleDraft({scope,text:'saved',...draft}),saved=await store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,copyInstance,binding:'',native:'',...captured,request:{key:'2'.repeat(16),text:'saved',anchor:0,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]});
 await store.decidePeoplePublication({scope,publication:saved.publication,expected:recoveryExpected(db,saved.publication),proof:{state:'locally_discarded'}});const decision=db.recovery.get('decision:'+saved.publication),modified=structuredClone(decision);if(change==='text')modified.request.text='other';else modified[change]=change==='state'?'completed':'other';db.recovery.set(decision.key,modified);
 const before=JSON.stringify([...db.recovery.values()]),files=await store.list(scope);assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),false);assert.equal(JSON.stringify([...db.recovery.values()]),before);assert.deepEqual(await store.list(scope),files);
});

for(const text of ['saved text','different edit']) test(`text retirement preserves a newer ${text==='saved text'?'same':'different'} text head and cannot complete late`,async()=>{
 const f=await textRetirementContractFixture(),newer=await f.store.savePeopleDraft({scope:f.scope,text,head:f.draft.head,expected:f.draft.version}),args={scope:f.scope,publication:f.publication,expected:f.expected,current:()=>true};
 assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');const result=await f.store.retirePeopleTextPublication(args);assert.equal(result.state,'retired');const before=JSON.stringify([...f.db.recovery.values()]);
 await assert.rejects(()=>f.store.finishPeopleTextPublication({...args,seq:8}),/identity|retired|changed/i);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);const heads=(await f.store.listPeopleRecovery(f.scope)).heads;assert.ok(heads.some(row=>row.head===newer.head&&row.version===newer.version&&row.text===text));
});

for(const field of ['scope','publication','expected','current','peopleKey','copyInstance','binding','native','head','version','request','request.key','request.text','request.anchor']) test(`text retirement rejects wrong-typed ${field} without mutation`,async()=>{
 const f=await textRetirementContractFixture(),args={scope:f.scope,publication:f.publication,expected:structuredClone(f.expected),current:()=>true};
 const target=field.startsWith('request.')?args.expected.request:['scope','publication','expected','current'].includes(field)?args:args.expected,key=field.startsWith('request.')?field.slice(8):field;target[key]=key==='anchor'?'0':42;
 const before=JSON.stringify([...f.db.recovery.values()]),files=await f.store.list(f.scope);assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');
 await assert.rejects(()=>f.store.retirePeopleTextPublication(args),/identity|unavailable|invalid|recovery/i);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);assert.deepEqual(await f.store.list(f.scope),files);
});

for(const mutation of ['unknown-kind','unknown-state','wrong-scope','wrong-head','wrong-version','wrong-request','conflicting-decision']) test(`text retirement refuses ${mutation} durable record without mutation`,async()=>{
 const f=await textRetirementContractFixture(),key='text-publication:'+f.publication,row=structuredClone(f.db.recovery.get(key));
 if(mutation==='unknown-kind')row.kind='future-record';if(mutation==='unknown-state')row.state='unknown';if(mutation==='wrong-scope')row.scope='other';if(mutation==='wrong-head')row.head='other';if(mutation==='wrong-version')row.version='other';if(mutation==='wrong-request')row.request.key='9'.repeat(16);f.db.recovery.set(key,row);
 if(mutation==='conflicting-decision')f.db.recovery.set('text-decision:'+f.publication,{...row,key:'text-decision:'+f.publication,kind:'text-decision',state:'completed',seq:9});
 const before=JSON.stringify([...f.db.recovery.values()]);assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');
 await assert.rejects(()=>f.store.retirePeopleTextPublication({scope:f.scope,publication:f.publication,expected:f.expected,current:()=>true}),/identity|conflict|changed|unavailable/i);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);
});

for(const component of [0,1,2,3]) test(`text retirement fences People address component ${component}`,async()=>{
 const f=await textRetirementContractFixture(),expected=structuredClone(f.expected),parts=expected.peopleKey.split('/');parts[component]=(parts[component][0]==='a'?'f':'a').repeat(parts[component].length);expected.peopleKey=parts.join('/');
 const before=JSON.stringify([...f.db.recovery.values()]);assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');await assert.rejects(()=>f.store.retirePeopleTextPublication({scope:f.scope,publication:f.publication,expected,current:()=>true}),/identity|conflict|changed/i);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);
});

test('text retirement rechecks current when a queued transaction enters',async()=>{
 let files=[],recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}],tail=Promise.resolve(),gate=null;
 const writePeople=fn=>{const hold=gate;gate=null;const op=tail.then(async()=>{if(hold){hold.entered();await hold.wait;}const result=fn({files,recovery});files=result.files;recovery=result.recovery;return result.value;});tail=op.catch(()=>{});return op;};
 const store=createPeopleFileStore({read:async()=>files,readWrite:fn=>writePeople(({files,recovery})=>{const r=fn(files);return {files:r.rows,recovery,value:r.value};}),writePeople}),scope='people\0synthetic\0',draft=await store.savePeopleDraft({scope,text:'saved'}),expected={peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',...draft,request:{key:'2'.repeat(16),text:'saved',anchor:0}},created=await store.beginPeopleTextPublication({scope,...expected});
 assert.equal(typeof store.retirePeopleTextPublication,'function','future text retirement API is not implemented');let release,entered,current=true;const ready=new Promise(resolve=>{entered=resolve;}),wait=new Promise(resolve=>{release=resolve;});gate={wait,entered};const before=JSON.stringify(recovery),flight=store.retirePeopleTextPublication({scope,publication:created.publication,expected,current:()=>current});await ready;current=false;release();await assert.rejects(flight,/cancel|current|closed/i);assert.equal(JSON.stringify(recovery),before);
});

test('committed text retirement stays durable when its caller loses current afterwards',async()=>{
 const f=await textRetirementContractFixture();assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');let current=true;const result=await f.store.retirePeopleTextPublication({scope:f.scope,publication:f.publication,expected:f.expected,current:()=>current});current=false;
 assert.equal(result.state,'retired');const row=f.db.recovery.get('text-decision:'+f.publication);assert.equal(row.state,'retired');assert.deepEqual(row.request,f.expected.request);assert.ok(f.db.recovery.has('head:'+f.captured.head));assert.equal(current,false);
});

test('text retirement releases its pending reservation while preserving unrelated pending reservation and file bytes',async()=>{
 const f=await textRetirementContractFixture(),otherScope='people\0other\0',otherDraft=await f.store.savePeopleDraft({scope:otherScope,text:'other'}),other=await f.store.beginPeopleTextPublication({scope:otherScope,peopleKey:recoveryPeopleKey,copyInstance:'2'.repeat(32),binding:'other',native:'',...otherDraft,request:{key:'4'.repeat(16),text:'other',anchor:0}}),files=await f.store.list(f.scope),allocator=structuredClone(f.db.recovery.get('allocator'));
 assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');assert.equal((await f.store.retirePeopleTextPublication({scope:f.scope,publication:f.publication,expected:f.expected,current:()=>true})).state,'retired');assert.equal(f.db.recovery.get('text-publication:'+other.publication).state,'pending');assert.deepEqual(f.db.recovery.get('allocator'),allocator);assert.deepEqual(await f.store.list(f.scope),files);assert.equal(f.db.recovery.get('text-decision:'+f.publication).request.key,f.expected.request.key);
});

test('text retirement frees only its reserved recovery entry at the 2048-entry boundary',async()=>{
 const f=await textRetirementContractFixture(),scope='people\0second\0',draft=await f.store.savePeopleDraft({scope,text:'another'}),identity={scope,peopleKey:recoveryPeopleKey,copyInstance:'2'.repeat(32),binding:'second',native:'',...draft,request:{key:'5'.repeat(16),text:'another',anchor:0}};
 while(f.db.recovery.size<2046){const key='capacity:'+f.db.recovery.size;f.db.recovery.set(key,{key,kind:'synthetic-padding'});}assert.equal(f.db.recovery.size,2046);await assert.rejects(f.store.beginPeopleTextPublication(identity),/limit/i);
 assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');const files=await f.store.list(f.scope);assert.equal((await f.store.retirePeopleTextPublication({scope:f.scope,publication:f.publication,expected:f.expected,current:()=>true})).state,'retired');const pending=await f.store.beginPeopleTextPublication(identity);assert.equal(f.db.recovery.size,2047);assert.equal(f.db.recovery.get('text-publication:'+pending.publication).state,'pending');assert.equal(f.db.recovery.get('text-decision:'+f.publication).state,'retired');assert.deepEqual(await f.store.list(f.scope),files);
});

test('text retirement releases its pending slot without releasing other file-publication slots',async()=>{
 const f=await textRetirementContractFixture(),id='3'.repeat(16),row=(await f.store.list(f.scope))[0],revision='a'.repeat(64);await f.store.setStageRevision(f.scope,id,revision,row.fileToken);
 for(let i=0;i<15;i++)await f.store.beginPeoplePublication({scope:f.scope,peopleKey:recoveryPeopleKey,copyInstance:f.expected.copyInstance,binding:f.expected.binding,native:'',...f.draft,request:{key:(i+16).toString(16).padStart(16,'0'),text:'saved text',anchor:0,attachments:[id]},files:[{id,fileToken:row.fileToken,stageRevision:revision}]});
 const other=await f.store.savePeopleDraft({scope:f.scope,text:'another'}),args={scope:f.scope,peopleKey:recoveryPeopleKey,copyInstance:f.expected.copyInstance,binding:f.expected.binding,native:'',...other,request:{key:'f'.repeat(16),text:'another',anchor:0}};await assert.rejects(f.store.beginPeopleTextPublication(args),/unconfirmed|limit/i);
 assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');await f.store.retirePeopleTextPublication({scope:f.scope,publication:f.publication,expected:f.expected,current:()=>true});const created=await f.store.beginPeopleTextPublication(args);assert.equal(f.db.recovery.get('text-publication:'+created.publication).state,'pending');assert.equal([...f.db.recovery.values()].filter(row=>row.kind==='publication'&&row.state==='pending').length,15);assert.equal((await f.store.list(f.scope))[0].fileToken,row.fileToken);
});

for(const field of ['scope','publication','expected','current']) test(`text retirement rejects absent top-level ${field}`,async()=>{
 const f=await textRetirementContractFixture(),args={scope:f.scope,publication:f.publication,expected:f.expected,current:()=>true};delete args[field];const before=JSON.stringify([...f.db.recovery.values()]);assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');await assert.rejects(()=>f.store.retirePeopleTextPublication(args),/identity|invalid|unavailable|recovery/i);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);
});

test('text retirement rejects a stale snapshot even when a terminal decision already exists',async()=>{
 const f=await textRetirementContractFixture(),args={scope:f.scope,publication:f.publication,expected:f.expected,current:()=>true};assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');assert.equal((await f.store.retirePeopleTextPublication(args)).state,'retired');const before=JSON.stringify([...f.db.recovery.values()]),changed=structuredClone(f.expected);changed.request.text='other';await assert.rejects(()=>f.store.retirePeopleTextPublication({...args,expected:changed}),/identity|conflict|changed/i);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);
});

test('text retirement releases its measured 8 MiB reservation without changing unrelated records',async()=>{
 const f=await textRetirementContractFixture(),scope='people\0other\0',draft=await f.store.savePeopleDraft({scope,text:'other'}),request={key:'5'.repeat(16),text:'other',anchor:0},identity={scope,peopleKey:recoveryPeopleKey,copyInstance:'2'.repeat(32),binding:'other',native:'',...draft,request},enc=new TextEncoder(),size=row=>enc.encode(row.key).length+enc.encode(JSON.stringify(row)).length,allocator=f.db.recovery.get('allocator'),publication=allocator.epoch+':'+allocator.next,row={key:'text-publication:'+publication,kind:'text-publication',state:'pending',scope,peopleKey:identity.peopleKey,copyInstance:identity.copyInstance,binding:identity.binding,native:identity.native,head:identity.head,version:identity.version,publication,request},reserve=65536,increment=size(row)+reserve+size({...allocator,next:allocator.next+1})-size(allocator);
 let bytes=[...f.db.recovery.values()].reduce((n,item)=>n+size(item)+(item.kind==='text-publication'&&item.state==='pending'?reserve:0),0),target=8*1024*1024-increment+1,i=0;
 while(bytes<target){const key='metadata-padding:'+i++,base={key,kind:'synthetic-padding',data:''},length=Math.min(64000,target-bytes-size(base));assert.ok(length>=0);const item={...base,data:'x'.repeat(length)};f.db.recovery.set(key,item);bytes+=size(item);}assert.equal(bytes,target);await assert.rejects(f.store.beginPeopleTextPublication(identity),/limit/i);
 assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');const files=await f.store.list(f.scope);await f.store.retirePeopleTextPublication({scope:f.scope,publication:f.publication,expected:f.expected,current:()=>true});const created=await f.store.beginPeopleTextPublication(identity);assert.equal(f.db.recovery.get('text-publication:'+created.publication).state,'pending');assert.equal(f.db.recovery.get('text-decision:'+f.publication).state,'retired');assert.deepEqual(await f.store.list(f.scope),files);
});

test('text retirement checks false current before driver transaction admission',async()=>{
 const f=await textRetirementContractFixture(),driver=createIndexedDBPeopleDriver(f.db);let admissions=0,currentCalls=0;const store=createPeopleFileStore({...driver,writePeople(...args){admissions++;return driver.writePeople(...args);}}),before=JSON.stringify([...f.db.recovery.values()]),files=await f.store.list(f.scope);
 assert.equal(typeof store.retirePeopleTextPublication,'function','future text retirement API is not implemented');
 await assert.rejects(()=>store.retirePeopleTextPublication({scope:f.scope,publication:f.publication,expected:f.expected,current:()=>{currentCalls++;return false;}}),/cancel|current|closed/i);
 assert.ok(currentCalls>=1);assert.equal(admissions,0,'a stale operation must not enter the transaction driver');assert.equal(JSON.stringify([...f.db.recovery.values()]),before);assert.deepEqual(await f.store.list(f.scope),files);
});

test('text retirement checks current again after admission when queued',async()=>{
 let files=[],recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}],release,entered,hold=false,admissions=0,current=true,currentCalls=0;const ready=new Promise(resolve=>{entered=resolve;});
 const writePeople=async fn=>{admissions++;if(hold){entered();await new Promise(resolve=>{release=resolve;});}const next=fn({files,recovery});files=next.files;recovery=next.recovery;return next.value;};
 const store=createPeopleFileStore({read:async()=>files,readWrite:async fn=>{const next=fn(files);files=next.rows;return next.value;},writePeople}),scope='people\0synthetic\0',draft=await store.savePeopleDraft({scope,text:'saved'}),expected={peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'binding',native:'',...draft,request:{key:'2'.repeat(16),text:'saved',anchor:0}},created=await store.beginPeopleTextPublication({scope,...expected});
 assert.equal(typeof store.retirePeopleTextPublication,'function','future text retirement API is not implemented');const before=JSON.stringify(recovery),prior=admissions;hold=true;const flight=store.retirePeopleTextPublication({scope,publication:created.publication,expected,current:()=>{currentCalls++;return current;}});await ready;assert.equal(admissions,prior+1);assert.ok(currentCalls>=1,'the pre-admission check must run before entering the driver');const checkedBefore=currentCalls;current=false;release();await assert.rejects(flight,/cancel|current|closed/i);assert.ok(currentCalls>checkedBefore,'the transaction must recheck ownership');assert.equal(JSON.stringify(recovery),before);
});

for(const state of ['retired','completed']) for(const defect of ['row-key','absent-row','kind','state','publication','scope','peopleKey','copyInstance','binding','native','head','version','request-key','request-text','request-anchor',...(state==='completed'?['sequence-absent','sequence-zero','sequence-negative','sequence-string','sequence-fraction']:[])]) test(`text retirement rejects ${state} terminal ${defect} without mutation`,async()=>{
 const f=await textRetirementContractFixture(),pendingKey='text-publication:'+f.publication,decisionKey='text-decision:'+f.publication,row=structuredClone(f.db.recovery.get(pendingKey));f.db.recovery.delete(pendingKey);Object.assign(row,{key:decisionKey,kind:'text-decision',state,...(state==='completed'?{seq:7}:{})});
 if(defect==='row-key')row.key='text-decision:other';if(defect==='kind')row.kind='unknown';if(defect==='state')row.state='unknown';if(defect==='publication')row.publication='f'.repeat(32)+':1';if(defect==='scope')row.scope='other';if(defect==='peopleKey')row.peopleKey='f'.repeat(32)+'/'+row.peopleKey.split('/').slice(1).join('/');if(defect==='copyInstance')row.copyInstance='f'.repeat(32);if(defect==='binding'||defect==='native'||defect==='head'||defect==='version')row[defect]='other';if(defect==='request-key')row.request.key='f'.repeat(16);if(defect==='request-text')row.request.text='other';if(defect==='request-anchor')row.request.anchor=1;
 if(defect==='sequence-absent')delete row.seq;if(defect==='sequence-zero')row.seq=0;if(defect==='sequence-negative')row.seq=-1;if(defect==='sequence-string')row.seq='7';if(defect==='sequence-fraction')row.seq=1.5;
 if(defect!=='absent-row')f.db.recovery.set(decisionKey,row);const before=JSON.stringify([...f.db.recovery.values()]),files=await f.store.list(f.scope);
 assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');await assert.rejects(()=>f.store.retirePeopleTextPublication({scope:f.scope,publication:f.publication,expected:f.expected,current:()=>true}),/identity|conflict|changed|invalid|unavailable|recovery/i);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);assert.deepEqual(await f.store.list(f.scope),files);
});

for(const terminal of ['retired','completed']) test(`text retirement refuses simultaneous pending and ${terminal} decision`,async()=>{
 const f=await textRetirementContractFixture(),key='text-decision:'+f.publication,pending=f.db.recovery.get('text-publication:'+f.publication);f.db.recovery.set(key,{...structuredClone(pending),key,kind:'text-decision',state:terminal,...(terminal==='completed'?{seq:7}:{})});const before=JSON.stringify([...f.db.recovery.values()]);
 assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');await assert.rejects(()=>f.store.retirePeopleTextPublication({scope:f.scope,publication:f.publication,expected:f.expected,current:()=>true}),/identity|conflict|changed|invalid|recovery/i);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);
});

test('text retirement accepts the compatible empty copy instance and preserves its identity',async()=>{
 const db=fakeIndexedDB(),store=createBrowserPeopleFileStore(db),scope='people\0synthetic\0',draft=await store.savePeopleDraft({scope,text:'saved'}),expected={peopleKey:recoveryPeopleKey,copyInstance:'',binding:'',native:'',...draft,request:{key:'2'.repeat(16),text:'saved',anchor:0}},created=await store.beginPeopleTextPublication({scope,...expected}),before=[...db.recovery.values()].map(row=>structuredClone(row)),args={scope,publication:created.publication,expected,current:()=>true};
 assert.equal(typeof store.retirePeopleTextPublication,'function','future text retirement API is not implemented');assert.equal((await store.retirePeopleTextPublication(args)).state,'retired');const decision=db.recovery.get('text-decision:'+created.publication),original=before.find(row=>row.key==='text-publication:'+created.publication);assert.deepEqual(decision,{...original,key:'text-decision:'+created.publication,kind:'text-decision',state:'retired'});assert.equal(decision.copyInstance,'');assert.deepEqual([...db.recovery.values()].filter(row=>row!==decision),before.filter(row=>row!==original));
});

for(const terminal of ['retired','completed']) test(`exact ${terminal} text retirement repeat is immutable and rejects different identity`,async()=>{
 const f=await textRetirementContractFixture(),args={scope:f.scope,publication:f.publication,expected:f.expected,current:()=>true};assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');if(terminal==='completed')await f.store.finishPeopleTextPublication({...args,seq:9});
 const first=await f.store.retirePeopleTextPublication(args),before=JSON.stringify([...f.db.recovery.values()]);assert.equal(first.state,terminal);if(terminal==='completed')assert.equal(first.seq,9);assert.deepEqual(await f.store.retirePeopleTextPublication(args),first);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);
 const changed=structuredClone(f.expected);changed.request.key='f'.repeat(16);await assert.rejects(()=>f.store.retirePeopleTextPublication({...args,expected:changed}),/identity|conflict|changed/i);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);
});

test('text retirement refuses an unavailable transaction capability',async()=>{
 const f=await textRetirementContractFixture(),store=createPeopleFileStore({read:async()=>[],readWrite:async fn=>fn([]).value}),before=JSON.stringify([...f.db.recovery.values()]);assert.equal(typeof store.retirePeopleTextPublication,'function','future text retirement API is not implemented');
 await assert.rejects(()=>store.retirePeopleTextPublication({scope:f.scope,publication:f.publication,expected:f.expected,current:()=>true}),/unavailable|recovery/i);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);
});

for(const field of ['request','request.key','request.text','request.anchor']) test(`text retirement rejects absent nested ${field}`,async()=>{
 const f=await textRetirementContractFixture(),expected=structuredClone(f.expected);if(field==='request')delete expected.request;else delete expected.request[field.slice(8)];const before=JSON.stringify([...f.db.recovery.values()]);assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');await assert.rejects(()=>f.store.retirePeopleTextPublication({scope:f.scope,publication:f.publication,expected,current:()=>true}),/identity|invalid|unavailable|recovery/i);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);
});

for(const defect of ['row-key','absent-row','kind','state','publication','missing-publication']) test(`text retirement rejects inconsistent pending ${defect} without mutation`,async()=>{
 const f=await textRetirementContractFixture(),key='text-publication:'+f.publication,row=structuredClone(f.db.recovery.get(key));f.db.recovery.delete(key);if(defect==='row-key')row.key='text-publication:other';if(defect==='kind')row.kind='unknown';if(defect==='state')row.state='unknown';if(defect==='publication')row.publication='f'.repeat(32)+':1';if(defect==='missing-publication')delete row.publication;if(defect!=='absent-row')f.db.recovery.set(key,row);const before=JSON.stringify([...f.db.recovery.values()]);
 assert.equal(typeof f.store.retirePeopleTextPublication,'function','future text retirement API is not implemented');await assert.rejects(()=>f.store.retirePeopleTextPublication({scope:f.scope,publication:f.publication,expected:f.expected,current:()=>true}),/identity|conflict|changed|invalid|unavailable|recovery/i);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);
});

async function retainedTextDiscoveryFixture() {
 const f=await textRetirementContractFixture(),key=recoveryPeopleKey,copy=f.expected.copyInstance,native='';
 const add=async({scope,binding,peopleKey=key,copyInstance=copy,native:owner=native,text='other',requestKey='4'.repeat(16)})=>{const draft=await f.store.savePeopleDraft({scope,text}),saved=await f.store.beginPeopleTextPublication({scope,peopleKey,copyInstance,binding,native:owner,...draft,request:{key:requestKey,text,anchor:0}});return saved.publication;};
 const old=await add({scope:'people\0old\0',binding:'old'}),otherCopy=await add({scope:'people\0copy\0',binding:'old',copyInstance:'5'.repeat(32)}),otherNative=await add({scope:'people\0native\0',binding:'old',native:'other'}),different=[];
 for(let i=0;i<4;i++){const parts=key.split('/');parts[i]='f'.repeat(parts[i].length);different.push(await add({scope:'people\0component'+i+'\0',binding:'old',peopleKey:parts.join('/')}));}
 const retired=await add({scope:'people\0retired\0',binding:'old'}),retiredRow=f.db.recovery.get('text-publication:'+retired);await f.store.retirePeopleTextPublication({scope:retiredRow.scope,publication:retired,expected:retiredRow,current:()=>true});
 const completed=await add({scope:'people\0completed\0',binding:'old'}),completedRow=f.db.recovery.get('text-publication:'+completed);await f.store.finishPeopleTextPublication({scope:completedRow.scope,publication:completed,expected:completedRow,seq:7});
 const id='3'.repeat(16),file=(await f.store.list(f.scope))[0],revision='a'.repeat(64);await f.store.setStageRevision(f.scope,id,revision,file.fileToken);const filePub=await f.store.beginPeoplePublication({scope:f.scope,peopleKey:key,copyInstance:copy,binding:'binding',native,...f.draft,request:{key:'6'.repeat(16),text:'saved text',anchor:0,attachments:[id]},files:[{id,fileToken:file.fileToken,stageRevision:revision}]});
 return {...f,key,copy,native,old,otherCopy,otherNative,different,retired,completed,filePublication:filePub.publication};
}

test('retained People recovery default and explicit file kind keep the same file rows',async()=>{
 const f=await retainedTextDiscoveryFixture(),args={peopleKey:f.key,copyInstance:f.copy,native:f.native},before=JSON.stringify([...f.db.recovery.values()]),files=await f.store.list(f.scope),implicit=await f.store.listRetainedPeopleRecovery(args),explicit=await f.store.listRetainedPeopleRecovery({...args,kind:'publication'});
 assert.deepEqual(implicit,explicit);assert.deepEqual(implicit.map(row=>row.publication),[f.filePublication]);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);assert.deepEqual(await f.store.list(f.scope),files);
});

test('retained text discovery selects exact identity across bindings and returns independent clones',async()=>{
 const f=await retainedTextDiscoveryFixture(),args={peopleKey:f.key,copyInstance:f.copy,native:f.native,kind:'text-publication'},before=JSON.stringify([...f.db.recovery.values()]),files=await f.store.list(f.scope),rows=await f.store.listRetainedPeopleRecovery(args);
 assert.deepEqual(rows.map(row=>row.publication).sort(),[f.publication,f.old].sort());assert.deepEqual(rows.map(row=>row.binding).sort(),['binding','old']);rows[0].request.text='caller mutation';rows[0].binding='changed';const again=await f.store.listRetainedPeopleRecovery(args);assert.ok(again.every(row=>row.request.text!=='caller mutation'&&row.binding!=='changed'));assert.equal(JSON.stringify([...f.db.recovery.values()]),before);assert.deepEqual(await f.store.list(f.scope),files);
});

for(const field of ['peopleKey','copyInstance','native']) test(`retained text discovery fences ${field}`,async()=>{
 const f=await retainedTextDiscoveryFixture(),args={peopleKey:f.key,copyInstance:f.copy,native:f.native,kind:'text-publication'};args[field]=field==='peopleKey'?f.key.replace(/^a/,'f'):field==='copyInstance'?'9'.repeat(32):'wrong';const before=JSON.stringify([...f.db.recovery.values()]);assert.deepEqual(await f.store.listRetainedPeopleRecovery(args),[]);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);
});

test('retained text supports empty copy identity while file discovery refuses it',async()=>{
 const f=await retainedTextDiscoveryFixture(),scope='people\0no-copy\0',draft=await f.store.savePeopleDraft({scope,text:'compatible'}),saved=await f.store.beginPeopleTextPublication({scope,peopleKey:f.key,copyInstance:'',binding:'old',native:'',...draft,request:{key:'9'.repeat(16),text:'compatible',anchor:0}}),before=JSON.stringify([...f.db.recovery.values()]);
 const rows=await f.store.listRetainedPeopleRecovery({peopleKey:f.key,copyInstance:'',native:'',kind:'text-publication'});assert.deepEqual(rows.map(row=>row.publication),[saved.publication]);await assert.rejects(f.store.listRetainedPeopleRecovery({peopleKey:f.key,copyInstance:'',native:'',kind:'publication'}),/identity|unavailable/i);await assert.rejects(f.store.listRetainedPeopleRecovery({peopleKey:f.key,copyInstance:'',native:''}),/identity|unavailable/i);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);
});

for(const kind of [null,42,{},'unknown','text-decision','']) test(`retained People recovery rejects invalid kind ${JSON.stringify(kind)}`,async()=>{
 const f=await retainedTextDiscoveryFixture(),before=JSON.stringify([...f.db.recovery.values()]);await assert.rejects(f.store.listRetainedPeopleRecovery({peopleKey:f.key,copyInstance:f.copy,native:'',kind}),/identity|unavailable|invalid/i);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);
});

for(const terminal of ['retired','completed']) test(`text admission refuses a ${terminal} captured identity but allows a fresh capture`,async()=>{
 const f=await textRetirementContractFixture(),args={scope:f.scope,publication:f.publication,expected:f.expected,current:()=>true};if(terminal==='retired')await f.store.retirePeopleTextPublication(args);else {const id='3'.repeat(16),file=(await f.store.list(f.scope))[0],revision='a'.repeat(64);await f.store.setStageRevision(f.scope,id,revision,file.fileToken);await f.store.beginPeoplePublication({scope:f.scope,peopleKey:f.expected.peopleKey,copyInstance:f.expected.copyInstance,binding:f.expected.binding,native:f.expected.native,...f.captured,request:{key:'6'.repeat(16),text:'saved text',anchor:0,attachments:[id]},files:[{id,fileToken:file.fileToken,stageRevision:revision}]});await f.store.finishPeopleTextPublication({...args,seq:7});}
 assert.ok(f.db.recovery.has('head:'+f.captured.head));assert.ok(f.db.recovery.has('draft:'+f.captured.version));const before=JSON.stringify([...f.db.recovery.values()]),files=await f.store.list(f.scope),stale={scope:f.scope,...f.expected,request:{...f.expected.request,key:'7'.repeat(16)}};await assert.rejects(f.store.beginPeopleTextPublication(stale),/retired|completed|identity|review|changed/i);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);assert.deepEqual(await f.store.list(f.scope),files);
 const draft=await f.store.savePeopleDraft({scope:f.scope,text:'fresh'}),fresh=await f.store.freezePeopleDraft({scope:f.scope,text:'fresh',...draft}),next=await f.store.beginPeopleTextPublication({scope:f.scope,peopleKey:f.expected.peopleKey,copyInstance:f.expected.copyInstance,binding:f.expected.binding,native:f.expected.native,...fresh,request:{key:'8'.repeat(16),text:'fresh',anchor:0}});assert.notEqual(next.publication,f.publication);assert.equal(f.db.recovery.get('text-decision:'+f.publication).state,terminal);assert.deepEqual(await f.store.list(f.scope),files);
});

for(const component of [0,1,2,3]) test(`retained text discovery separates People identity component ${component}`,async()=>{
 const f=await retainedTextDiscoveryFixture(),parts=f.key.split('/');parts[component]='f'.repeat(parts[component].length);const rows=await f.store.listRetainedPeopleRecovery({peopleKey:parts.join('/'),copyInstance:f.copy,native:'',kind:'text-publication'});assert.deepEqual(rows.map(row=>row.publication),[f.different[component]]);assert.ok(rows.every(row=>row.peopleKey===parts.join('/')));
});

test('retained text discovery excludes unknown-state and unknown-kind rows',async()=>{
 const f=await retainedTextDiscoveryFixture(),scope='people\0unknown\0',draft=await f.store.savePeopleDraft({scope,text:'unknown'}),unknown=await f.store.beginPeopleTextPublication({scope,peopleKey:f.key,copyInstance:f.copy,binding:'old',native:'',...draft,request:{key:'a'.repeat(16),text:'unknown',anchor:0}}),row=f.db.recovery.get('text-publication:'+unknown.publication);f.db.recovery.set(row.key,{...row,state:'unknown'});const before=JSON.stringify([...f.db.recovery.values()]);let rows=await f.store.listRetainedPeopleRecovery({peopleKey:f.key,copyInstance:f.copy,native:'',kind:'text-publication'});assert.ok(!rows.some(item=>item.publication===unknown.publication));assert.equal(JSON.stringify([...f.db.recovery.values()]),before);f.db.recovery.set(row.key,{...row,kind:'future-kind'});const beforeKind=JSON.stringify([...f.db.recovery.values()]);rows=await f.store.listRetainedPeopleRecovery({peopleKey:f.key,copyInstance:f.copy,native:'',kind:'text-publication'});assert.ok(!rows.some(item=>item.publication===unknown.publication));assert.equal(JSON.stringify([...f.db.recovery.values()]),beforeKind);
});

test('unknown text decision protects its exact captured identity from readmission',async()=>{
 const f=await textRetirementContractFixture(),args={scope:f.scope,publication:f.publication,expected:f.expected,current:()=>true};assert.equal((await f.store.retirePeopleTextPublication(args)).state,'retired');const key='text-decision:'+f.publication,decision=f.db.recovery.get(key);f.db.recovery.set(key,{...decision,state:'future-terminal'});
 assert.ok(f.db.recovery.has('head:'+f.captured.head));assert.ok(f.db.recovery.has('draft:'+f.captured.version));const before=JSON.stringify([...f.db.recovery.values()]),files=await f.store.list(f.scope),stale={scope:f.scope,...f.expected,request:{...f.expected.request,key:'7'.repeat(16)}};
 await assert.rejects(f.store.beginPeopleTextPublication(stale),/identity|review|changed|decided/i);assert.equal(JSON.stringify([...f.db.recovery.values()]),before);assert.deepEqual(await f.store.list(f.scope),files);
 const draft=await f.store.savePeopleDraft({scope:f.scope,text:'fresh'}),capture=await f.store.freezePeopleDraft({scope:f.scope,text:'fresh',...draft}),fresh=await f.store.beginPeopleTextPublication({scope:f.scope,peopleKey:f.expected.peopleKey,copyInstance:f.expected.copyInstance,binding:f.expected.binding,native:f.expected.native,...capture,request:{key:'8'.repeat(16),text:'fresh',anchor:0}});assert.notEqual(fresh.publication,f.publication);assert.equal(f.db.recovery.get(key).state,'future-terminal');assert.deepEqual(await f.store.list(f.scope),files);
});

async function redundantCaptureDecisionFixture(kind, identityOverrides={}) {
 const db=fakeIndexedDB(),store=createBrowserPeopleFileStore(db),scope='people\0synthetic\0',copyInstance='1'.repeat(32),draft=await store.savePeopleDraft({scope,text:'saved'}),captured=await store.freezePeopleDraft({scope,text:'saved',...draft}),newer=await store.savePeopleDraft({scope,text:'newer input'}),request={key:'2'.repeat(16),text:'saved',anchor:0},identity={scope,peopleKey:recoveryPeopleKey,copyInstance,binding:'',native:'',...captured,request,...identityOverrides};
 const id='3'.repeat(16),token=await store.save(scope,id,{name:'kept.txt',type:'text/plain',size:1}),revision='a'.repeat(64);await store.setStageRevision(scope,id,revision,token);
 let publication;
 if(kind==='text'){publication=await store.beginPeopleTextPublication(identity);await store.retirePeopleTextPublication({scope,publication:publication.publication,expected:identity,current:()=>true});}
 else {publication=await store.beginPeoplePublication({...identity,request:{...request,attachments:[id]},files:[{id,fileToken:token,stageRevision:revision}]});await store.decidePeoplePublication({scope,publication:publication.publication,expected:recoveryExpected(db,publication.publication),proof:{state:'locally_discarded'}});}
 return {db,store,scope,draft,captured,newer,id,token,revision,publication:publication.publication,decisionKey:(kind==='text'?'text-decision:':'decision:')+publication.publication};
}

for(const kind of ['text','file']) test(`exact retired ${kind} decision permits redundant captured draft cleanup only`,async()=>{
 const x=await redundantCaptureDecisionFixture(kind),{db,store,scope,captured,decisionKey}=x,before=structuredClone([...db.recovery.values()]),files=await store.list(scope),decision=structuredClone(db.recovery.get(decisionKey));
 assert.equal(decision.state,'retired');assert.equal(decision.head,captured.head);assert.equal(decision.version,captured.version);assert.equal(decision.request.text,'saved');
 assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),true);assert.deepEqual([...db.recovery.values()],before.filter(row=>row.key!=='head:'+captured.head&&row.key!=='draft:'+captured.version));assert.deepEqual(db.recovery.get(decisionKey),decision);assert.deepEqual(await store.list(scope),files);assert.ok(db.recovery.has('head:'+x.newer.head));assert.ok(db.recovery.has('draft:'+x.newer.version));
});

for(const kind of ['text','file']) for(const defect of ['unknown-state','wrong-key','wrong-publication','wrong-scope','wrong-head','wrong-version','wrong-text','wrong-request-key','wrong-anchor']) test(`captured draft refuses malformed retired ${kind} decision ${defect}`,async()=>{
 const x=await redundantCaptureDecisionFixture(kind),{db,store,scope,captured,decisionKey}=x,row=structuredClone(db.recovery.get(decisionKey));
 if(defect==='unknown-state')row.state='unknown';if(defect==='wrong-key')row.key='malformed';if(defect==='wrong-publication')row.publication='invalid';if(defect==='wrong-scope')row.scope='other';if(defect==='wrong-head')row.head='other';if(defect==='wrong-version')row.version='other';if(defect==='wrong-text')row.request.text='different';if(defect==='wrong-request-key')row.request.key='invalid';if(defect==='wrong-anchor')row.request.anchor='invalid';db.recovery.set(decisionKey,row);
 const before=structuredClone([...db.recovery.values()]),files=await store.list(scope);assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),false);assert.deepEqual([...db.recovery.values()],before);assert.deepEqual(await store.list(scope),files);
});

for(const kind of ['text','file']) for(const field of ['head','version','sourceHead','sourceVersion']) test(`retired ${kind} capture remains protected by unknown ${field} reference`,async()=>{
 const x=await redundantCaptureDecisionFixture(kind),{db,store,scope,captured}=x;db.recovery.set('future:reference',{key:'future:reference',kind:'future-record',scope,[field]:field.toLowerCase().includes('version')?captured.version:captured.head});const before=structuredClone([...db.recovery.values()]),files=await store.list(scope);
 assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),false);assert.deepEqual([...db.recovery.values()],before);assert.deepEqual(await store.list(scope),files);
});

test('retired text capture cleanup is not blocked by an unrelated file publication using the retained file',async()=>{
 const x=await redundantCaptureDecisionFixture('text'),{db,store,scope,captured}=x,other=await store.savePeopleDraft({scope,text:'other'}),request={key:'4'.repeat(16),text:'other',anchor:0,attachments:[x.id]},pending=await store.beginPeoplePublication({scope,peopleKey:recoveryPeopleKey,copyInstance:'1'.repeat(32),binding:'',native:'',...other,request,files:[{id:x.id,fileToken:x.token,stageRevision:x.revision}]}),before=structuredClone([...db.recovery.values()]),files=await store.list(scope);
 assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),true);assert.deepEqual([...db.recovery.values()],before.filter(row=>row.key!=='head:'+captured.head&&row.key!=='draft:'+captured.version));assert.equal(db.recovery.get('publication:'+pending.publication).state,'pending');assert.deepEqual(await store.list(scope),files);
});

for(const kind of ['text','file']) test(`retired ${kind} capture refuses a current loser without changing any row`,async()=>{
 const x=await redundantCaptureDecisionFixture(kind),{db,store,scope,captured}=x,before=structuredClone([...db.recovery.values()]),files=await store.list(scope);assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>false}),false);assert.deepEqual([...db.recovery.values()],before);assert.deepEqual(await store.list(scope),files);
});

for(const kind of ['text','file']) for(const state of ['pending','completed','unknown']) test(`captured draft refuses ${state} ${kind} publication reference`,async()=>{
 const x=await redundantCaptureDecisionFixture(kind),{db,store,scope,captured,decisionKey}=x,row=structuredClone(db.recovery.get(decisionKey));if(state==='pending'){db.recovery.delete(decisionKey);row.kind=kind==='text'?'text-publication':'publication';row.key=row.kind+':'+x.publication;}row.state=state;if(state==='completed')row.seq=7;db.recovery.set(row.key,row);const before=structuredClone([...db.recovery.values()]),files=await store.list(scope);
 assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),false);assert.deepEqual([...db.recovery.values()],before);assert.deepEqual(await store.list(scope),files);
});

for(const kind of ['text','file']) test(`failed retired ${kind} capture transaction preserves every row and file`,async()=>{
 const x=await redundantCaptureDecisionFixture(kind),{db,store,scope,captured}=x,before=structuredClone([...db.recovery.values()]),files=await store.list(scope),transaction=db.db.transaction;let aborted=0;
 db.db.transaction=function(...args){const tx=transaction.apply(this,args),objectStore=tx.objectStore;tx.objectStore=function(name){const result=objectStore.call(this,name);if(name==='recovery'){const remove=result.delete;result.delete=key=>{if(key==='head:'+captured.head){aborted++;queueMicrotask(()=>tx.abort());return {};}return remove.call(result,key);};}return result;};return tx;};
 try{await assert.rejects(store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),/unavailable/i);assert.equal(aborted,1);assert.deepEqual([...db.recovery.values()],before);assert.deepEqual(await store.list(scope),files);}finally{db.db.transaction=transaction;store.destroy();}
});

for(const kind of ['text','file']) for(const field of ['version','sourceHead','sourceVersion']) test(`retired ${kind} capture remains protected by another real head ${field}`,async()=>{
 const x=await redundantCaptureDecisionFixture(kind),{db,store,scope,captured}=x;db.recovery.set('head:synthetic-other',{key:'head:synthetic-other',kind:'head',scope,head:'synthetic-other',version:'synthetic-other-version',[field]:field==='sourceHead'?captured.head:captured.version});const before=structuredClone([...db.recovery.values()]),files=await store.list(scope);assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),false);assert.deepEqual([...db.recovery.values()],before);assert.deepEqual(await store.list(scope),files);
});

// A retired decision may release only its redundant capture when its frozen
// publication metadata is well formed. These corruptions are synthetic.
for (const kind of ['text','file']) for (const [label, mutate] of [
 ['people key shape', row => { row.peopleKey='bad'; }],
 ['copy instance shape', row => { row.copyInstance='bad'; }],
 ['binding type', row => { row.binding=7; }],
 ['native type', row => { row.native=7; }],
 ['missing request', row => { delete row.request; }],
 ['negative anchor', row => { row.request.anchor=-1; }],
 ['fractional anchor', row => { row.request.anchor=0.5; }],
 ...(kind==='file' ? [
  ['binding length', row => { row.binding='b'.repeat(257); }],
  ['native length', row => { row.native='n'.repeat(257); }],
  ['empty copy instance', row => { row.copyInstance=''; }],
  ['missing attachment IDs', row => { delete row.request.attachments; }],
  ['empty attachment IDs', row => { row.request.attachments=[]; }],
  ['oversize attachment IDs', row => { row.request.attachments=Array.from({length:17},(_,i)=>i.toString(16).padStart(16,'0')); }],
  ['malformed attachment ID', row => { row.request.attachments=['bad']; }],
  ['duplicate attachment IDs', row => { row.request.attachments=[row.files[0].id,row.files[0].id]; }],
  ['missing manifest', row => { delete row.files; }],
  ['empty manifest', row => { row.files=[]; }],
  ['oversize manifest', row => { row.files=Array.from({length:17},()=>({...row.files[0]})); }],
  ['duplicate manifest', row => { row.files=[row.files[0],{...row.files[0]}]; }],
  ['manifest count', row => { row.files.push({...row.files[0],id:'4'.repeat(16)}); }],
  ['manifest order', row => { const first=row.files[0]; row.request.attachments=[first.id,'4'.repeat(16)]; row.files=[{...first,id:'4'.repeat(16)},first]; }],
  ['file token', row => { row.files[0].fileToken='bad'; }],
  ['stage revision', row => { row.files[0].stageRevision='bad'; }],
 ] : []),
]) test(`retired ${kind} capture rejects malformed ${label} without data loss`,async()=>{
 const x=await redundantCaptureDecisionFixture(kind), {db,store,scope,captured,decisionKey}=x;
 const row=structuredClone(db.recovery.get(decisionKey)); mutate(row); db.recovery.set(decisionKey,row);
 const before=structuredClone([...db.recovery.values()]), files=await store.list(scope);
 const result=await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true});
 assert.deepEqual([...db.recovery.values()],before); assert.deepEqual(await store.list(scope),files);
 assert.ok(db.recovery.has('head:'+x.newer.head)); assert.ok(db.recovery.has('draft:'+x.newer.version));
 assert.equal(result,false);
});

for (const kind of ['text','file']) test(`retired ${kind} capture accepts valid opaque native and old binding`,async()=>{
 const x=await redundantCaptureDecisionFixture(kind), {db,store,scope,captured,decisionKey}=x;
 const row=structuredClone(db.recovery.get(decisionKey)); row.native='opaque:old'; row.binding='old binding';
 if(kind==='text') row.copyInstance=''; db.recovery.set(decisionKey,row);
 const before=structuredClone([...db.recovery.values()]),files=await store.list(scope);
 assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),true);
 assert.deepEqual([...db.recovery.values()],before.filter(r=>r.key!=='head:'+captured.head&&r.key!=='draft:'+captured.version));
 assert.deepEqual(await store.list(scope),files);
});

test('retired file capture cleanup does not require already removed file bytes',async()=>{
 const x=await redundantCaptureDecisionFixture('file'),{db,store,scope,captured,decisionKey}=x;
 await store.remove(scope,x.id);const before=structuredClone([...db.recovery.values()]),files=await store.list(scope);
 assert.equal(files.length,0);assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),true);
 assert.deepEqual([...db.recovery.values()],before.filter(row=>row.key!=='head:'+captured.head&&row.key!=='draft:'+captured.version));
 assert.ok(db.recovery.has(decisionKey));assert.deepEqual(await store.list(scope),files);
});

for (const field of ['binding','native']) test(`retired text capture accepts long ${field} supported by admission`,async()=>{
 const x=await redundantCaptureDecisionFixture('text',{[field]:'x'.repeat(257)}),{db,store,scope,captured,decisionKey}=x;
 assert.equal(db.recovery.get(decisionKey)[field],'x'.repeat(257));
 const before=structuredClone([...db.recovery.values()]),files=await store.list(scope);
 assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),true);
 assert.deepEqual([...db.recovery.values()],before.filter(r=>r.key!=='head:'+captured.head&&r.key!=='draft:'+captured.version));
 assert.deepEqual(await store.list(scope),files);
});
for (const kind of ['text','file']) for (const field of ['peopleKey','copyInstance']) test(`retired ${kind} capture rejects nonstring ${field}`,async()=>{
 const x=await redundantCaptureDecisionFixture(kind),{db,store,scope,captured,decisionKey}=x;
 const row=structuredClone(db.recovery.get(decisionKey));row[field]=null;db.recovery.set(decisionKey,row);
 const before=structuredClone([...db.recovery.values()]),files=await store.list(scope);
 assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),false);
 assert.deepEqual([...db.recovery.values()],before);assert.deepEqual(await store.list(scope),files);
});
test('retired file capture rejects a null frozen manifest',async()=>{
 const x=await redundantCaptureDecisionFixture('file'),{db,store,scope,captured,decisionKey}=x;
 const row=structuredClone(db.recovery.get(decisionKey));row.files=null;db.recovery.set(decisionKey,row);
 const before=structuredClone([...db.recovery.values()]),files=await store.list(scope);
 assert.equal(await store.discardPeopleCapture({scope,...captured,text:'saved',current:()=>true}),false);
 assert.deepEqual([...db.recovery.values()],before);assert.deepEqual(await store.list(scope),files);
});
