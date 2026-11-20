import test from 'node:test';
import assert from 'node:assert/strict';
import * as review from '../js/people-local-review.js';
const makeKey=review.peoplePendingStorageKey || (()=>'');
const parse=review.parsePeoplePendingSnapshot || (()=>null);
const key=['a'.repeat(32),'b'.repeat(32),'c'.repeat(16),'d'.repeat(16)].join('/'),copyInstance='e'.repeat(32),instance='f'.repeat(32),binding='old-binding';
const identity={key,copyInstance,instance,binding},id='1'.repeat(16),stage='2'.repeat(16);
const payload=(change={})=>({instance,copyInstance,request:{key:id,text:'synthetic',anchor:0,attachments:[stage],...change}});
test('pending key constructor and parser preserve the exact captured snapshot',()=>{
  const storageKey=`scimux-people-file-pending:${key}:${instance}:${binding}:copy:${copyInstance}`,value=JSON.stringify(payload());
  assert.equal(makeKey(identity),storageKey);assert.equal(makeKey({...identity,binding:''}),`scimux-people-file-pending:${key}:${instance}:copy:${copyInstance}`);
  const result=parse(value,identity);assert.deepEqual(result,{key,copyInstance,binding,instance,storageKey,value,publicationKey:id,text:'synthetic',attachments:[stage]});result.attachments.push('3'.repeat(16));assert.equal(JSON.parse(value).request.attachments.length,1);
});
test('pending parser rejects damaged records without fabricating authority',()=>{
  const bad=['{','null','{}',JSON.stringify({...payload(),instance:'wrong'}),JSON.stringify({...payload(),copyInstance:'wrong'}),...[
    {key:null},{key:'A'.repeat(16)},{text:null},{text:'é'.repeat(8193)},{anchor:-1},{anchor:1.5},{anchor:Number.MAX_SAFE_INTEGER+1},{attachments:null},{attachments:[]},{attachments:Array(17).fill(stage)},{attachments:[stage,stage]},{attachments:['A'.repeat(16)]},{attachments:[null]}
  ].map(c=>JSON.stringify(payload(c)))];
  for(const raw of bad)assert.throws(()=>parse(raw,identity),undefined,raw);
  const boundary=JSON.stringify(payload({text:'é'.repeat(8192),attachments:Array.from({length:16},(_,i)=>i.toString(16).padStart(16,'0'))}));assert.equal(parse(boundary,identity).attachments.length,16);
});
