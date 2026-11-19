import test from "node:test";
import assert from "node:assert/strict";
import {createPeopleFileStore,createIndexedDBPeopleDriver,createBrowserPeopleFileStore} from "../js/people-file-store.js";

function fakeDriver() {
 let rows=[];
 return {async readWrite(fn){const result=fn(rows.map(row=>({...row})));rows=result.rows;return result.value;},async read(){return rows.map(row=>({...row}));}};
}

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

function fakeIndexedDB() {
 const records=new Map();let created=false;
 const db={objectStoreNames:{contains:()=>created},createObjectStore(){created=true;},close(){},transaction(_name,mode){
  const working=new Map(records);let pending=0;let aborted=false;let scheduled=false;
  const tx={error:null,abort(){aborted=true;queueMicrotask(()=>tx.onabort?.());},objectStore(){return {
   getAll(){return request(()=>[...working.values()]);},
   put(row){return request(()=>{working.set(row.key,row);return row.key;});},
   delete(key){return request(()=>{working.delete(key);});},
  }}};
  function finish(){if(scheduled)return;scheduled=true;queueMicrotask(()=>{scheduled=false;if(pending || aborted)return;if(mode==="readwrite"){records.clear();for(const [k,v] of working)records.set(k,v)}tx.oncomplete?.();});}
  function request(fn){const req={};pending++;queueMicrotask(()=>{if(aborted)return;try{req.result=fn();req.onsuccess?.()}catch(err){req.error=err;tx.error=err;req.onerror?.();tx.abort()}finally{pending--;finish()}});return req;}
  return tx;
 }};
 return {db,open(){const req={};queueMicrotask(()=>{req.result=db;if(!created)req.onupgradeneeded?.();req.onsuccess?.()});return req;}};
}

test("browser People storage is optional but uses IndexedDB when available", async()=>{
 assert.equal(createBrowserPeopleFileStore(null),null);
 const store=createBrowserPeopleFileStore(fakeIndexedDB());
 await store.save("scope","0123456789abcdef",{name:"x",type:"text/plain",size:1});
 assert.equal((await store.list("scope")).length,1);
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
