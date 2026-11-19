import test from "node:test";
import assert from "node:assert/strict";
import {preparePeopleFiles} from "../js/people-preparation.js";

test("source file preparation carries the selected copy identity", async()=>{
 const source={server:"a".repeat(32),owner:"b".repeat(32),chat:"c".repeat(16),generation:"d".repeat(16),log:"comments",seq:1};
 const file={id:"0123456789abcdef",sha256:"e".repeat(64),name:"synthetic.txt",mime:"text/plain",size:1},copyInstance="f".repeat(32);let requested="";
 await preparePeopleFiles({source,attachments:[file],copyInstance,fetchImpl:async path=>{requested=path;return {ok:true,arrayBuffer:async()=>new ArrayBuffer(1)}},digest:async()=>file.sha256,makeFile:()=>({})});
 assert.equal(new URL(requested,"http://local.invalid").searchParams.get("copy_instance"),copyInstance);
});

test("explicit owner preparation reads only verified exact source associations", async()=>{
 const source={server:"a".repeat(32),owner:"b".repeat(32),chat:"c".repeat(16),generation:"d".repeat(16),log:"comments",seq:7};
 const file={id:"0123456789abcdef",sha256:"e".repeat(64),name:"synthetic.txt",mime:"text/plain",size:2};const calls=[];
 const files=await preparePeopleFiles({source,attachments:[file],fetchImpl:async(path,opts)=>{calls.push({path,opts});return {ok:true,arrayBuffer:async()=>new Uint8Array([1,2]).buffer}},digest:async()=>file.sha256,makeFile:(bytes,name,opts)=>({bytes,name,...opts})});
 assert.equal(calls.length,1);assert.equal(calls[0].opts.method,"GET");assert.equal(calls[0].path,`/api/team/subscriptions/${source.server}/${source.owner}/${source.chat}/${source.generation}/attachments/7/${file.id}`);assert.equal(files[0].name,file.name);
});

test("preparation rejects forged descriptors and corrupt bytes without agent operations", async()=>{
 const source={server:"a".repeat(32),owner:"b".repeat(32),chat:"c".repeat(16),generation:"d".repeat(16),log:"comments",seq:7};
 const file={id:"0123456789abcdef",sha256:"e".repeat(64),name:"synthetic.txt",mime:"text/plain",size:2};let calls=0;
 const deps={source,attachments:[file],fetchImpl:async()=>{calls++;return {ok:true,arrayBuffer:async()=>new Uint8Array([1,2]).buffer}},digest:async()=>"f".repeat(64),makeFile:()=>{throw Error("must not stage corrupt bytes")}};
 await assert.rejects(preparePeopleFiles({...deps,attachments:[{...file,name:"../escape"}]}),/unavailable/);assert.equal(calls,0);
 await assert.rejects(preparePeopleFiles(deps),/corrupt/);assert.equal(calls,1);
});

test('preparation rejects invalid source, file identities, limits and names before fetching', async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};
 const file={id:'0123456789abcdef',sha256:'e'.repeat(64),name:'safe.txt',mime:'text/plain',size:1};let requests=0;
 const base={source,attachments:[file],fetchImpl:async()=>{requests++;return {ok:true,arrayBuffer:async()=>new ArrayBuffer(1)}},digest:async()=>file.sha256,makeFile:()=>({})};
 const cases=[{source:null},{source:{...source,server:3}},{source:{...source,owner:''}},{source:{...source,chat:'x'}},{source:{...source,generation:'x'}},{source:{...source,log:'mirror'}},{source:{...source,seq:0}},{source:{...source,seq:1.5}},{attachments:null},{attachments:[]},{attachments:Array(17).fill(file)},{fetchImpl:null},{digest:null},{makeFile:null},
  ...[null,{...file,id:'z'},{...file,sha256:'x'},{...file,size:0},{...file,size:4194305},{...file,size:1.5},{...file,name:''},{...file,name:'.'},{...file,name:'..'},{...file,name:'a/b'},{...file,name:'a\\b'},{...file,name:'x\ny'},{...file,name:'é'.repeat(128)},{...file,mime:null},{...file,mime:'x'.repeat(256)}].map(item=>({attachments:[item]})),{attachments:[file,file]}];
 for(const change of cases) await assert.rejects(preparePeopleFiles({...base,...change}),/unavailable/);
 const large=Array.from({length:5},(_,i)=>({...file,id:String(i).padStart(16,'0'),size:4194304}));
 await assert.rejects(preparePeopleFiles({...base,attachments:large}),/unavailable/);assert.equal(requests,0);
});

test('preparation stops on failed requests, invalid bytes and retired operations at each await',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};
 const file={id:'0123456789abcdef',sha256:'e'.repeat(64),name:'a.txt',mime:'text/plain',size:1};
 const base={source,attachments:[file],fetchImpl:async()=>({ok:true,arrayBuffer:async()=>new ArrayBuffer(1)}),digest:async()=>file.sha256,makeFile:()=>({})};
 await assert.rejects(preparePeopleFiles({...base,fetchImpl:async()=>({ok:false})}),/unavailable/);
 await assert.rejects(preparePeopleFiles({...base,fetchImpl:async()=>({ok:true,arrayBuffer:async()=>new ArrayBuffer(0)})}),/corrupt/);
 for(const max of [0,1,2]) {let calls=0;await assert.rejects(preparePeopleFiles({...base,current:()=>calls++<max}),/Source changed/);}
 const abort=new AbortController();abort.abort();await assert.rejects(preparePeopleFiles({...base,signal:abort.signal}),/Source changed/);
});

test('preparation cancellation after either download or digest cannot stage a file',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};
 const file={id:'0123456789abcdef',sha256:'e'.repeat(64),name:'a.txt',mime:'text/plain',size:1};
 for(const phase of ['download','digest']) {
  const controller=new AbortController();let staged=0;
  const fetchImpl=async()=>({ok:true,arrayBuffer:async()=>{if(phase==='download')controller.abort();return new ArrayBuffer(1)}});
  const digest=async()=>{if(phase==='digest')controller.abort();return file.sha256};
  await assert.rejects(preparePeopleFiles({source,attachments:[file],signal:controller.signal,fetchImpl,digest,makeFile:()=>{staged++}}),/Source changed/);assert.equal(staged,0);
 }
});

test('People file failures show safe actionable feedback and distinguish uncertainty',async()=>{
 const {peopleFileFailureText}=await import('../js/people-preparation.js');
 for(const [code,expected] of [['storage_full',/computer.*storage.*full/i],['too_large',/large/i],['read_only',/permission/i],['not_active',/changed|ended/i],['relay_quota',/relay.*(full|quota)/i],['file_too_large',/large/i],['relay_forbidden',/permission/i],['relay_unauthorized',/access/i],['stale_generation',/changed|ended/i],['channel_unavailable',/unavailable/i],['attachments_unsupported',/support/i],['upload_unconfirmed',/unconfirmed/i]]) assert.match(peopleFileFailureText(JSON.stringify({error:code,stage_id:'synthetic'})),expected);
 assert.match(peopleFileFailureText('private raw error and secret'),/unconfirmed/i);assert.doesNotMatch(peopleFileFailureText('private raw error and secret'),/secret/);
});

test('People file feedback treats absent error codes as unknown outcomes',async()=>{
 const {peopleFileFailureText}=await import('../js/people-preparation.js');
 for(const message of ['null','{}','{"error":""}']) assert.match(peopleFileFailureText(message),/unconfirmed/i);
});

test('copy identity errors explain review and reconnection',async()=>{
 const {peopleFileFailureText}=await import('../js/people-preparation.js');
 for(const code of ['copy_instance_required','copy_changed','copy_identity_unavailable']) assert.match(peopleFileFailureText(JSON.stringify({error:code})),/copy|reconnect|refresh/i);
});

test('source digest uses SHA-256 and preserves leading zeroes in its hex encoding',async()=>{
 const {sha256Hex}=await import('../js/people-preparation.js');const input=new Uint8Array([1,2]);let called=0;
 const result=await sha256Hex(input,{digest:async(algorithm,bytes)=>{called++;assert.equal(algorithm,'SHA-256');assert.equal(bytes,input);return new Uint8Array([0,1,15,16,255]).buffer}});
 assert.equal(result,'00010f10ff');assert.equal(called,1);
 const {webcrypto}=await import('node:crypto');assert.equal(await sha256Hex(new TextEncoder().encode('abc'),webcrypto.subtle),'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad');
});
