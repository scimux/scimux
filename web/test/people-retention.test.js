import test from "node:test";
import assert from "node:assert/strict";
import {retainPeopleEntries} from "../js/people-retention.js";

test("retention carries the local copy identity without dispatching", async()=>{
 const key=["a".repeat(32),"b".repeat(32),"c".repeat(16),"d".repeat(16)].join("/"),copyInstance="e".repeat(32);let requested="";
 const result=await retainPeopleEntries({key,copyInstance,entries:[{seq:1,attachments:[{id:"0123456789abcdef",size:1}]}],fetchImpl:async path=>{requested=path;return {ok:true}}});
 assert.equal(new URL(requested,"http://local.invalid").searchParams.get("copy_instance"),copyInstance);assert.equal(result.available,1);
});

test("receiving People files fetches only exact local associations and never dispatches", async()=>{
 const key=["a".repeat(32),"b".repeat(32),"c".repeat(16),"d".repeat(16)].join("/");
 const calls=[];const result=await retainPeopleEntries({key,entries:[{seq:2,attachments:[{id:"0123456789abcdef",size:3}]}],fetchImpl:async(path,opts)=>{calls.push({path,opts});return {ok:true,body:{cancel:async()=>{}}}}});
 assert.deepEqual(calls.map(call=>call.path),[`/api/team/subscriptions/${key}/attachments/2/0123456789abcdef`]);
 assert.equal(calls[0].opts.method,"GET");assert.equal(result.available,1);assert.equal(result.unavailable,0);
});

test("retention times out even when an adapter does not resolve after abort", async()=>{
 const key=["a".repeat(32),"b".repeat(32),"c".repeat(16),"d".repeat(16)].join("/");
 const work=retainPeopleEntries({key,entries:[{seq:1,attachments:[{id:"0123456789abcdef",size:1}]}],fetchImpl:()=>new Promise(()=>{}),timeoutMs:5});
 const result=await Promise.race([work,new Promise(resolve=>setTimeout(()=>resolve("stuck"),50))]);
 assert.notEqual(result,"stuck");assert.equal(result.unavailable,1);
});

test("retention reports unavailable associations individually", async()=>{
 const key=["a".repeat(32),"b".repeat(32),"c".repeat(16),"d".repeat(16)].join("/");
 const result=await retainPeopleEntries({key,entries:[{seq:1,attachments:[{id:"0123456789abcdef",size:1},{id:"fedcba9876543210",size:1}]}],fetchImpl:async path=>({ok:path.endsWith("0123456789abcdef"),body:{cancel:async()=>{}}})});
 assert.equal(result.files["1/0123456789abcdef"],true);assert.equal(result.files["1/fedcba9876543210"],false);
});

test('retention ignores malformed entries, duplicate associations and invalid inputs',async()=>{
 const key=['a'.repeat(32),'b'.repeat(32),'c'.repeat(16),'d'.repeat(16)].join('/');let calls=0;
 const fetchImpl=async()=>{calls++;return {ok:true}};
 for(const args of [{key:null,entries:[]},{key:'bad',entries:[]},{key,entries:null},{key,entries:[],fetchImpl:null}]) {
  const result=await retainPeopleEntries({fetchImpl,...args});assert.equal(result.available,0);assert.equal(result.unavailable,0);
 }
 const file={id:'0123456789abcdef',size:1};const entries=[null,{seq:0,attachments:[file]},{seq:1.5,attachments:[file]},{seq:1,attachments:null},{seq:1,attachments:Array(17).fill(file)},{seq:2,attachments:[null,{...file,id:1},{...file,id:'bad'},{...file,size:0},{...file,size:4194305},{...file,size:1.5},file,file]}];
 const result=await retainPeopleEntries({key,entries,fetchImpl});assert.equal(calls,1);assert.equal(result.available,1);assert.equal(result.files['2/'+file.id],true);
});

test('retention reports cancellation and transport errors without granting availability',async()=>{
 const key=['a'.repeat(32),'b'.repeat(32),'c'.repeat(16),'d'.repeat(16)].join('/');const entries=[{seq:1,attachments:[{id:'0123456789abcdef',size:1}]}];
 const controller=new AbortController();controller.abort();let calls=0;
 const cancelled=await retainPeopleEntries({key,entries,signal:controller.signal,fetchImpl:async()=>{calls++;return {ok:true}}});assert.equal(cancelled.unavailable,1);assert.equal(calls,0);
 const failure=await retainPeopleEntries({key,entries,fetchImpl:async()=>{throw Error('synthetic network failure')}});assert.equal(failure.unavailable,1);assert.equal(failure.files['1/0123456789abcdef'],false);
});

test("a kept-cap refusal stays available and is marked unkept", async () => {
 const key = ["a".repeat(32), "b".repeat(32), "c".repeat(16), "d".repeat(16)].join("/");
 const headers = value => ({get: name => String(name).toLowerCase() === "x-scimux-people-kept" ? value : null});
 const entries = [{seq: 1, attachments: [{id: "0123456789abcdef", size: 1}, {id: "fedcba9876543210", size: 1}]}];
 const result = await retainPeopleEntries({key, entries, fetchImpl: async path => ({ok: true, headers: headers(path.endsWith("0123456789abcdef") ? "false" : null), body: {cancel: async () => {}}})});
 assert.equal(result.available, 2);
 assert.equal(result.unavailable, 0);
 assert.equal(result.files["1/0123456789abcdef"], "unkept");
 assert.equal(result.files["1/fedcba9876543210"], true);
 const other = await retainPeopleEntries({key, entries: [{seq: 1, attachments: [{id: "0123456789abcdef", size: 1}]}], fetchImpl: async () => ({ok: true, headers: headers("False"), body: {cancel: async () => {}}})});
 assert.equal(other.files["1/0123456789abcdef"], true);
 const refused = await retainPeopleEntries({key, entries: [{seq: 1, attachments: [{id: "0123456789abcdef", size: 1}]}], fetchImpl: async () => ({ok: false, headers: headers("false")})});
 assert.equal(refused.available, 0);
 assert.equal(refused.unavailable, 1);
 assert.equal(refused.files["1/0123456789abcdef"], false);
});

test('retention skips empty pages and handles abort while finishing a response',async()=>{
 const key=['a'.repeat(32),'b'.repeat(32),'c'.repeat(16),'d'.repeat(16)].join('/');let requests=0;
 const empty=await retainPeopleEntries({key,entries:[],fetchImpl:async()=>{requests++}});assert.equal(empty.available,0);assert.equal(requests,0);
 const controller=new AbortController();const entries=[{seq:1,attachments:[{id:'0123456789abcdef',size:1}]}];
 const result=await retainPeopleEntries({key,entries,signal:controller.signal,fetchImpl:async()=>({ok:true,body:{cancel:async()=>{controller.abort()}}})});
 assert.equal(result.available,0);assert.equal(result.unavailable,1);assert.equal(result.files['1/0123456789abcdef'],false);
});
